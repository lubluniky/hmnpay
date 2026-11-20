// Package evmtest provides a scripted, in-memory EVM chain and JSON-RPC nodes
// serving it, for tests of the evm client and of the payment watcher.
//
// A Chain is the "real" chain: tests mine blocks on it, emit token transfers
// and reorganise it. A Node is one RPC endpoint looking at a Chain; it can be
// told to misbehave the way real endpoints do — lag, fail, rate-limit, cap
// eth_getLogs ranges, refuse eth_getLogs altogether, silently omit logs, or
// stay stuck on a fork.
//
// Nothing here is meant for production use.
package evmtest

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
)

// RawLog is an event log before it is placed in a block.
type RawLog struct {
	Address ethaddr.Address
	Topics  []evm.Hash
	Data    []byte
}

// Tx is a transaction: a hash and the logs it emits.
type Tx struct {
	Hash evm.Hash
	Logs []RawLog
	// Failed marks a reverted transaction (receipt status 0, no logs).
	Failed bool
}

// Block is a mined block. Blocks are immutable once mined.
type Block struct {
	Number     uint64
	Hash       evm.Hash
	ParentHash evm.Hash
	Time       uint64
	Txs        []Tx
}

type token struct {
	symbol   string
	decimals uint8
	balances map[ethaddr.Address]*big.Int
}

// Chain is a scripted chain. It is safe for concurrent use.
type Chain struct {
	mu      sync.RWMutex
	chainID uint64
	base    uint64 // number of blocks[0]
	blocks  []Block
	pending []Tx
	tokens  map[ethaddr.Address]*token
	salt    uint64 // mixed into block hashes; bumped on every reorg
	nextTx  uint64
	clock   uint64 // timestamp for the next blocks; 0: derived from the height
}

// NewChain creates a chain whose head is at the given height. Earlier blocks
// do not exist (as on a pruned node), which lets tests start at realistic
// heights without creating millions of blocks.
func NewChain(chainID, height uint64) *Chain {
	c := &Chain{chainID: chainID, base: height, tokens: make(map[ethaddr.Address]*token)}
	var parent evm.Hash
	binary.BigEndian.PutUint64(parent[24:], height) // an arbitrary non-zero parent
	parent[0] = 0xba
	c.blocks = []Block{c.makeBlock(height, parent, nil)}
	return c
}

// SetTime sets the timestamp that blocks mined from now on carry (until it
// is set again). Without it a block's timestamp is derived from its height.
// Timestamps never go backwards: a block is never older than its parent.
func (c *Chain) SetTime(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clock = uint64(t.Unix())
}

// ChainID returns the chain id.
func (c *Chain) ChainID() uint64 { return c.chainID }

// AddToken registers an ERC-20 token contract so that eth_getCode and
// eth_call (symbol, decimals, balanceOf) answer for it.
func (c *Chain) AddToken(addr ethaddr.Address, symbol string, decimals uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[addr] = &token{symbol: symbol, decimals: decimals, balances: make(map[ethaddr.Address]*big.Int)}
}

// TransferLog builds the log of an ERC-20 Transfer event.
func TransferLog(tokenAddr, from, to ethaddr.Address, amount *big.Int) RawLog {
	data := make([]byte, 32)
	amount.FillBytes(data)
	return RawLog{
		Address: tokenAddr,
		Topics:  []evm.Hash{evm.TransferTopic, addressTopic(from), addressTopic(to)},
		Data:    data,
	}
}

func addressTopic(a ethaddr.Address) evm.Hash {
	var h evm.Hash
	copy(h[12:], a[:])
	return h
}

// Transfer queues a transaction with a single ERC-20 Transfer event; it is
// included in the next mined block. It returns the transaction hash.
func (c *Chain) Transfer(tokenAddr, from, to ethaddr.Address, amount *big.Int) evm.Hash {
	return c.Submit(TransferLog(tokenAddr, from, to, amount))
}

// Submit queues a transaction emitting the given logs and returns its hash.
func (c *Chain) Submit(logs ...RawLog) evm.Hash {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextTx++
	// The chain id is part of the hash, as it is part of every signed
	// transaction: two chains never produce the same transaction hash.
	var seed [18]byte
	copy(seed[:2], "tx")
	binary.BigEndian.PutUint64(seed[2:], c.chainID)
	binary.BigEndian.PutUint64(seed[10:], c.nextTx)
	tx := Tx{Hash: evm.Hash(ethaddr.Keccak256(seed[:])), Logs: logs}
	c.pending = append(c.pending, tx)
	return tx.Hash
}

// SubmitFailed queues a transaction that reverts: it is mined with status 0
// and emits nothing.
func (c *Chain) SubmitFailed() evm.Hash {
	h := c.Submit()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending[len(c.pending)-1].Failed = true
	return h
}

// Requeue puts transactions (typically the ones returned by Reorg) back into
// the pending set with their original hashes, so that the next mined block
// includes them again.
func (c *Chain) Requeue(txs ...Tx) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, txs...)
}

// Mine mines n blocks and returns the new head. All pending transactions go
// into the first of them.
func (c *Chain) Mine(n int) Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := 0; i < n; i++ {
		head := c.blocks[len(c.blocks)-1]
		b := c.makeBlock(head.Number+1, head.Hash, c.pending)
		c.pending = nil
		c.blocks = append(c.blocks, b)
	}
	return c.blocks[len(c.blocks)-1]
}

// Reorg removes the last depth blocks, as if a competing fork had won, and
// returns the transactions that were in them (oldest first). The removed
// transactions are not re-queued: call Requeue for those that should be mined
// again on the new fork. Blocks mined afterwards get different hashes than
// the removed ones even if their contents are identical.
func (c *Chain) Reorg(depth int) []Tx {
	c.mu.Lock()
	defer c.mu.Unlock()
	if depth >= len(c.blocks) {
		panic(fmt.Sprintf("evmtest: cannot reorg %d blocks of a %d-block chain", depth, len(c.blocks)))
	}
	cut := len(c.blocks) - depth
	var dropped []Tx
	for _, b := range c.blocks[cut:] {
		dropped = append(dropped, b.Txs...)
	}
	c.blocks = append([]Block(nil), c.blocks[:cut]...)
	c.salt++
	return dropped
}

// Head returns the latest block.
func (c *Chain) Head() Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.blocks[len(c.blocks)-1]
}

// BlockByNumber returns the canonical block with the given number.
func (c *Chain) BlockByNumber(n uint64) (Block, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if n < c.base || n-c.base >= uint64(len(c.blocks)) {
		return Block{}, false
	}
	return c.blocks[n-c.base], true
}

// Clone returns an independent copy of the chain in its current state. A Node
// pointed at the clone keeps serving this state while the original moves on
// (or is reorganised) — a node stuck on a stale fork.
func (c *Chain) Clone() *Chain {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cp := &Chain{
		chainID: c.chainID,
		base:    c.base,
		blocks:  append([]Block(nil), c.blocks...),
		pending: append([]Tx(nil), c.pending...),
		tokens:  make(map[ethaddr.Address]*token, len(c.tokens)),
		salt:    c.salt + 1000, // blocks mined on the clone differ from the original's
		nextTx:  c.nextTx + 1_000_000,
		clock:   c.clock,
	}
	for addr, t := range c.tokens {
		nt := &token{symbol: t.symbol, decimals: t.decimals, balances: make(map[ethaddr.Address]*big.Int, len(t.balances))}
		for a, b := range t.balances {
			nt.balances[a] = new(big.Int).Set(b)
		}
		cp.tokens[addr] = nt
	}
	return cp
}

// view returns the blocks visible to a node that lags by lag blocks.
func (c *Chain) view(lag uint64) (base uint64, blocks []Block) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := uint64(len(c.blocks))
	if lag >= n {
		lag = n - 1
	}
	return c.base, c.blocks[:n-lag]
}

func (c *Chain) tokenInfo(addr ethaddr.Address) (symbol string, decimals uint8, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tokens[addr]
	if !ok {
		return "", 0, false
	}
	return t.symbol, t.decimals, true
}

func (c *Chain) balanceOf(tokenAddr, owner ethaddr.Address) *big.Int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tokens[tokenAddr]
	if !ok {
		return new(big.Int)
	}
	if b, ok := t.balances[owner]; ok {
		return new(big.Int).Set(b)
	}
	return new(big.Int)
}

// blockTime returns the timestamp of the block about to be mined on top of
// the current chain. The caller holds c.mu.
func (c *Chain) blockTime(number uint64) uint64 {
	t := 1_700_000_000 + number
	if c.clock != 0 {
		t = c.clock
	}
	if n := len(c.blocks); n > 0 && c.blocks[n-1].Time > t {
		t = c.blocks[n-1].Time
	}
	return t
}

// makeBlock builds a block. The caller holds c.mu (or is the constructor).
func (c *Chain) makeBlock(number uint64, parent evm.Hash, txs []Tx) Block {
	var buf []byte
	buf = binary.BigEndian.AppendUint64(buf, c.salt)
	buf = binary.BigEndian.AppendUint64(buf, number)
	buf = append(buf, parent[:]...)
	for _, tx := range txs {
		buf = append(buf, tx.Hash[:]...)
	}
	// Credit balances of registered tokens (best effort bookkeeping for
	// balanceOf; reorgs do not roll it back, which no test relies on).
	for _, tx := range txs {
		if tx.Failed {
			continue
		}
		for _, l := range tx.Logs {
			t, ok := c.tokens[l.Address]
			if !ok || len(l.Topics) != 3 || l.Topics[0] != evm.TransferTopic || len(l.Data) != 32 {
				continue
			}
			var to ethaddr.Address
			copy(to[:], l.Topics[2][12:])
			bal, ok := t.balances[to]
			if !ok {
				bal = new(big.Int)
				t.balances[to] = bal
			}
			bal.Add(bal, new(big.Int).SetBytes(l.Data))
		}
	}
	return Block{
		Number:     number,
		Hash:       evm.Hash(ethaddr.Keccak256(buf)),
		ParentHash: parent,
		Time:       c.blockTime(number),
		Txs:        append([]Tx(nil), txs...),
	}
}
