package service

import (
	"bytes"
	"encoding/hex"
	"github.com/OpenBazaar/multiwallet/client"
	"github.com/OpenBazaar/multiwallet/keys"
	laddr "github.com/OpenBazaar/multiwallet/litecoin/address"
	"github.com/OpenBazaar/multiwallet/util"
	zaddr "github.com/OpenBazaar/multiwallet/zcash/address"
	"github.com/OpenBazaar/wallet-interface"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/cpacia/bchutil"
	"github.com/op/go-logging"
	"strconv"
	"sync"
	"time"
)

var log = logging.MustGetLogger("WalletService")

type WalletService struct {
	db       wallet.Datastore
	km       *keys.KeyManager
	client   client.APIClient
	params   *chaincfg.Params
	coinType wallet.CoinType

	chainHeight uint32
	bestBlock   string

	listeners []func(wallet.TransactionCallback)

	lock sync.RWMutex

	doneChan chan struct{}
}

const nullHash = "0000000000000000000000000000000000000000000000000000000000000000"

func NewWalletService(db wallet.Datastore, km *keys.KeyManager, client client.APIClient, params *chaincfg.Params, coinType wallet.CoinType) *WalletService {
	return &WalletService{db, km, client, params, coinType, 0, nullHash, []func(wallet.TransactionCallback){}, sync.RWMutex{}, make(chan struct{})}
}

func (ws *WalletService) Start() {
	log.Noticef("Starting %s WalletService", ws.coinType.String())
	go ws.UpdateState()
	go ws.listen()
}

func (ws *WalletService) Stop() {
	ws.doneChan <- struct{}{}
}

func (ws *WalletService) ChainTip() (uint32, chainhash.Hash) {
	ws.lock.RLock()
	defer ws.lock.RUnlock()
	ch, _ := chainhash.NewHashFromStr(ws.bestBlock)
	return uint32(ws.chainHeight), *ch
}

func (ws *WalletService) AddTransactionListener(callback func(callback wallet.TransactionCallback)) {
	ws.listeners = append(ws.listeners, callback)
}

func (ws *WalletService) listen() {
	addrs := ws.getStoredAddresses()
	for _, sa := range addrs {
		ws.client.ListenAddress(sa.Addr)
	}

	for {
		select {
		case <-ws.doneChan:
			break
		case tx := <-ws.client.TransactionNotify():
			go ws.processIncomingTransaction(tx)
		case block := <-ws.client.BlockNotify():
			go ws.processIncomingBlock(block)
		}
	}
}

// This is a transaction fresh off the wire. Let's save it to the db.
func (ws *WalletService) processIncomingTransaction(tx client.Transaction) {
	log.Debugf("New incoming %s transaction: %s", ws.coinType.String(), tx.Txid)
	addrs := ws.getStoredAddresses()
	ws.lock.RLock()
	chainHeight := int32(ws.chainHeight)
	ws.lock.RUnlock()
	ws.saveSingleTxToDB(tx, chainHeight, addrs)
	utxos, err := ws.db.Utxos().GetAll()
	if err != nil {
		log.Errorf("Error loading %s utxos: %s", ws.coinType.String(), err.Error())
	}

	for _, sa := range addrs {
		for _, out := range tx.Outputs {
			for _, addr := range out.ScriptPubKey.Addresses {
				if addr == sa.Addr.String() {
					utxo := client.Utxo{
						Txid:          tx.Txid,
						ScriptPubKey:  out.ScriptPubKey.Hex,
						Satoshis:      int64(out.Value * util.SatoshisPerCoin(ws.coinType)),
						Vout:          out.N,
						Address:       addr,
						Confirmations: 0,
						Amount:        out.Value,
					}
					ws.saveSingleUtxoToDB(utxo, addrs, chainHeight)
					break
				}
			}
		}
		// If spending a utxo, delete it
		for _, in := range tx.Inputs {
			for _, u := range utxos {
				if in.Txid == u.Op.Hash.String() && in.Vout == int(u.Op.Index) {
					ws.db.Utxos().Delete(u)
					break
				}
			}
		}
	}
}

// A new block was found let's update our chain height and best hash and check for a reorg
func (ws *WalletService) processIncomingBlock(block client.Block) {
	log.Infof("Received new %s block at height %d: %s", ws.coinType.String(), block.Height, block.Hash)
	ws.lock.RLock()
	currentBest := ws.bestBlock
	ws.lock.RUnlock()

	ws.lock.Lock()
	ws.chainHeight = uint32(block.Height)
	ws.bestBlock = block.Hash
	ws.lock.Unlock()

	// REORG! Rescan all transactions and utxos to see if anything changed
	if currentBest != block.PreviousBlockhash {
		log.Warningf("Reorg in the %s chain! Re-scanning wallet", ws.coinType.String())
		ws.UpdateState()
		return
	}

	// Query db for unconfirmed txs and utxos then query API to get current height
	txs, err := ws.db.Txns().GetAll(true)
	if err != nil {
		log.Errorf("Error loading %s txs from db: %s", ws.coinType.String(), err.Error())
		return
	}
	utxos, err := ws.db.Utxos().GetAll()
	if err != nil {
		log.Errorf("Error loading %s txs from db: %s", ws.coinType.String(), err.Error())
		return
	}
	addrs := ws.getStoredAddresses()
	for _, tx := range txs {
		if tx.Height == 0 {
			go func(txn wallet.Txn) {
				ret, err := ws.client.GetTransaction(txn.Txid)
				if err != nil {
					log.Errorf("Error fetching unconfirmed %s tx: %s", ws.coinType.String(), err.Error())
					return
				}
				if ret.Confirmations > 0 {
					h := int32(block.Height) - int32(ret.Confirmations-1)
					ws.saveSingleTxToDB(*ret, int32(block.Height), addrs)
					for _, u := range utxos {
						if u.Op.Hash.String() == txn.Txid {
							u.AtHeight = h
							ws.db.Utxos().Put(u)
						}
					}
				}
			}(tx)
		}
	}
}

// updateState will query the API for both UTXOs and TXs relevant to our wallet and then update
// the db state to match the API responses.
func (ws *WalletService) UpdateState() {
	// Start by fetching the chain height from the API
	log.Debugf("Querying for %s chain height", ws.coinType.String())
	best, err := ws.client.GetBestBlock()
	if err == nil {
		log.Debugf("%s chain height: %d", ws.coinType.String(), best.Height)
		ws.lock.Lock()
		ws.chainHeight = uint32(best.Height)
		ws.bestBlock = best.Hash
		ws.lock.Unlock()
	} else {
		log.Errorf("Error querying API for %s chain height: %s", ws.coinType.String(), err.Error())
	}

	// Load wallet addresses and watch only addresses from the db
	addrs := ws.getStoredAddresses()

	go ws.syncUtxos(addrs)
	go ws.syncTxs(addrs)

}

// Query API for UTXOs and synchronize db state
func (ws *WalletService) syncUtxos(addrs map[string]storedAddress) {
	log.Debugf("Querying for %s utxos", ws.coinType.String())
	var query []btcutil.Address
	for _, sa := range addrs {
		query = append(query, sa.Addr)
	}
	utxos, err := ws.client.GetUtxos(query)
	if err != nil {
		log.Errorf("Error downloading utxos for %s: %s", ws.coinType.String(), err.Error())
	} else {
		log.Debugf("Downloaded %d %s utxos", len(utxos), ws.coinType.String())
		ws.saveUtxosToDB(utxos, addrs)
	}
}

// For each API response we will have to figure out height at which the UTXO has confirmed (if it has) and
// build a UTXO object suitable for saving to the database. If the database contains any UTXOs not returned
// by the API we will delete them.
func (ws *WalletService) saveUtxosToDB(utxos []client.Utxo, addrs map[string]storedAddress) {
	// Get current utxos
	currentUtxos, err := ws.db.Utxos().GetAll()
	if err != nil {
		log.Error("Error loading utxos for %s: %s", ws.coinType.String(), err.Error())
		return
	}

	ws.lock.RLock()
	chainHeight := int32(ws.chainHeight)
	ws.lock.RUnlock()

	newUtxos := make(map[string]wallet.Utxo)
	// Iterate over new utxos and put them to the db
	for _, u := range utxos {
		ch, err := chainhash.NewHashFromStr(u.Txid)
		if err != nil {
			log.Error("Error converting to chainhash for %s: %s", ws.coinType.String(), err.Error())
			continue
		}
		newU := wallet.Utxo{
			Op: *wire.NewOutPoint(ch, uint32(u.Vout)),
		}
		newUtxos[serializeUtxo(newU)] = newU
		ws.saveSingleUtxoToDB(u, addrs, chainHeight)
	}
	// If any old utxos were not returned by the API, delete them.
	for _, cur := range currentUtxos {
		_, ok := newUtxos[serializeUtxo(cur)]
		if !ok {
			ws.db.Utxos().Delete(cur)
		}
	}
}

func (ws *WalletService) saveSingleUtxoToDB(u client.Utxo, addrs map[string]storedAddress, chainHeight int32) {
	ch, err := chainhash.NewHashFromStr(u.Txid)
	if err != nil {
		log.Error("Error converting to chainhash for %s: %s", ws.coinType.String(), err.Error())
		return
	}
	scriptBytes, err := hex.DecodeString(u.ScriptPubKey)
	if err != nil {
		log.Error("Error converting to script bytes for %s: %s", ws.coinType.String(), err.Error())
		return
	}

	var watchOnly bool
	sa, ok := addrs[u.Address]
	if sa.WatchOnly || !ok {
		watchOnly = true
	}

	height := int32(0)
	if u.Confirmations > 0 {
		height = chainHeight - (int32(u.Confirmations) - 1)
	}

	newU := wallet.Utxo{
		Op:           *wire.NewOutPoint(ch, uint32(u.Vout)),
		Value:        u.Satoshis,
		WatchOnly:    watchOnly,
		ScriptPubkey: scriptBytes,
		AtHeight:     height,
	}

	ws.db.Utxos().Put(newU)
}

// For use as a map key
func serializeUtxo(u wallet.Utxo) string {
	ser := u.Op.Hash.String()
	ser += strconv.Itoa(int(u.Op.Index))
	return ser
}

// Query API for TXs and synchronize db state
func (ws *WalletService) syncTxs(addrs map[string]storedAddress) {
	log.Debugf("Querying for %s transactions", ws.coinType.String())
	var query []btcutil.Address
	for _, sa := range addrs {
		query = append(query, sa.Addr)
	}
	txs, err := ws.client.GetTransactions(query)
	if err != nil {
		log.Errorf("Error downloading txs for %s: %s", ws.coinType.String(), err.Error())
	} else {
		log.Debugf("Downloaded %d %s transactions", len(txs), ws.coinType.String())
		ws.saveTxsToDB(txs, addrs)
	}
}

// For each API response we will need to determine the net coins leaving/entering the wallet as well as determine
// if the transaction was exclusively for our `watch only` addresses. We will also build a Tx object suitable
// for saving to the db and delete any existing txs not returned by the API. Finally, for any output matching a key
// in our wallet we need to mark that key as used in the db
func (ws *WalletService) saveTxsToDB(txns []client.Transaction, addrs map[string]storedAddress) {
	// Get current utxos
	currentTxs, err := ws.db.Txns().GetAll(true)
	if err != nil {
		log.Error("Error loading utxos for %s: %s", ws.coinType.String(), err.Error())
		return
	}

	ws.lock.RLock()
	chainHeight := int32(ws.chainHeight)
	ws.lock.RUnlock()

	newTxs := make(map[string]bool)
	// Iterate over new utxos and put them to the db
	for _, u := range txns {
		ws.saveSingleTxToDB(u, chainHeight, addrs)
		newTxs[u.Txid] = true
	}
	// If any old utxos were not returned by the API, delete them.
	for _, cur := range currentTxs {
		if !newTxs[cur.Txid] {
			ch, err := chainhash.NewHashFromStr(cur.Txid)
			if err != nil {
				log.Error("Error converting to chainhash for %s: %s", ws.coinType.String(), err.Error())
				continue
			}
			ws.db.Txns().Delete(ch)
		}
	}
}

func (ws *WalletService) saveSingleTxToDB(u client.Transaction, chainHeight int32, addrs map[string]storedAddress) {
	msgTx := wire.NewMsgTx(int32(u.Version))
	msgTx.LockTime = uint32(u.Locktime)
	hits := 0
	value := int64(0)

	height := int32(0)
	if u.Confirmations > 0 {
		height = chainHeight - (int32(u.Confirmations) - 1)
	}

	txHash, err := chainhash.NewHashFromStr(u.Txid)
	if err != nil {
		log.Error("Error converting to txHash for %s: %s", ws.coinType.String(), err.Error())
		return
	}
	var relevant bool
	cb := wallet.TransactionCallback{Txid: txHash.CloneBytes(), Height: height}
	for _, in := range u.Inputs {
		ch, err := chainhash.NewHashFromStr(in.Txid)
		if err != nil {
			log.Error("Error converting to chainhash for %s: %s", ws.coinType.String(), err.Error())
			continue
		}
		op := wire.NewOutPoint(ch, uint32(in.Vout))
		script, err := hex.DecodeString(in.ScriptSig.Hex)
		if err != nil {
			log.Error("Error converting to scriptSig for %s: %s", ws.coinType.String(), err.Error())
			continue
		}
		txin := wire.NewTxIn(op, script, [][]byte{})
		txin.Sequence = uint32(in.Sequence)
		msgTx.TxIn = append(msgTx.TxIn, txin)

		cbin := wallet.TransactionInput{
			OutpointHash:       op.Hash.CloneBytes(),
			OutpointIndex:      op.Index,
			LinkedScriptPubKey: script,
			Value:              in.Satoshis,
		}
		cb.Inputs = append(cb.Inputs, cbin)

		sa, ok := addrs[in.Addr]
		if !ok {
			continue
		}
		value -= in.Satoshis
		if !sa.WatchOnly {
			hits++
		}
		relevant = true
	}
	for i, out := range u.Outputs {
		script, err := hex.DecodeString(out.ScriptPubKey.Hex)
		if err != nil {
			log.Error("Error converting to scriptPubkey for %s: %s", ws.coinType.String(), err.Error())
			continue
		}
		if len(out.ScriptPubKey.Addresses) == 0 {
			continue
		}
		v := int64(out.Value * util.SatoshisPerCoin(ws.coinType))

		txout := wire.NewTxOut(v, script)
		msgTx.TxOut = append(msgTx.TxOut, txout)
		cbout := wallet.TransactionOutput{ScriptPubKey: script, Value: v, Index: uint32(i)}
		cb.Outputs = append(cb.Outputs, cbout)

		sa, ok := addrs[out.ScriptPubKey.Addresses[0]]
		if !ok {
			continue
		}
		value += v
		if !sa.WatchOnly {
			hits++
			// Mark the key we received coins to as used
			ws.db.Keys().MarkKeyAsUsed(sa.Addr.ScriptAddress())
		}
		relevant = true
	}

	if !relevant {
		return
	}

	cb.Value = value
	cb.WatchOnly = (hits == 0)
	saved, err := ws.db.Txns().Get(*txHash)
	if err != nil {
		ts := time.Now()
		if u.Confirmations > 0 {
			ts = time.Unix(u.BlockTime, 0)
		}
		var buf bytes.Buffer
		msgTx.BtcEncode(&buf, wire.ProtocolVersion, wire.WitnessEncoding)
		ws.db.Txns().Put(buf.Bytes(), msgTx.TxHash().String(), int(value), int(height), ts, hits == 0)
		cb.Timestamp = ts
		ws.callbackListeners(cb)
	} else {
		ws.db.Txns().UpdateHeight(*txHash, int(height), time.Unix(u.BlockTime, 0))
		if saved.Height != height {
			cb.Timestamp = saved.Timestamp
			ws.callbackListeners(cb)
		}
	}
}

func (ws *WalletService) callbackListeners(cb wallet.TransactionCallback) {
	for _, callback := range ws.listeners {
		callback(cb)
	}
}

type storedAddress struct {
	Addr      btcutil.Address
	WatchOnly bool
}

func (ws *WalletService) getStoredAddresses() map[string]storedAddress {
	keys := ws.km.GetKeys()
	addrs := make(map[string]storedAddress)
	for _, key := range keys {
		addr, err := ws.km.KeyToAddress(key)
		if err != nil {
			log.Errorf("Error getting %s address for key: %s", ws.coinType.String(), err.Error())
			continue
		}
		addrs[addr.String()] = storedAddress{addr, false}
	}
	watchScripts, err := ws.db.WatchedScripts().GetAll()
	if err != nil {
		log.Errorf("Error loading %s watch scripts: %s", ws.coinType.String(), err.Error())
	} else {
		for _, script := range watchScripts {
			switch ws.coinType {
			case wallet.Bitcoin:
				_, addrSlice, _, err := txscript.ExtractPkScriptAddrs(script, ws.params)
				if err != nil {
					log.Errorf("Error serializing %s script: %s", ws.coinType.String(), err.Error())
					continue
				}
				if len(addrs) == 0 {
					log.Errorf("Error serializing %s script: %s", ws.coinType.String(), "Unknown script")
					continue
				}
				addr := addrSlice[0]
				addrs[addr.String()] = storedAddress{addr, true}
			case wallet.BitcoinCash:
				addr, err := bchutil.ExtractPkScriptAddrs(script, ws.params)
				if err != nil {
					log.Errorf("Error serializing %s script: %s", ws.coinType.String(), err.Error())
					continue
				}
				addrs[addr.String()] = storedAddress{addr, true}
			case wallet.Zcash:
				addr, err := zaddr.ExtractPkScriptAddrs(script, ws.params)
				if err != nil {
					log.Errorf("Error serializing %s script: %s", ws.coinType.String(), err.Error())
					continue
				}
				addrs[addr.String()] = storedAddress{addr, true}
			case wallet.Litecoin:
				addr, err := laddr.ExtractPkScriptAddrs(script, ws.params)
				if err != nil {
					log.Errorf("Error serializing %s script: %s", ws.coinType.String(), err.Error())
					continue
				}
				addrs[addr.String()] = storedAddress{addr, true}
			}
		}
	}
	return addrs
}
