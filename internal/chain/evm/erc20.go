package evm

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"unicode"
	"unicode/utf8"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
)

// TransferTopic is topic0 of the ERC-20 event
// Transfer(address indexed from, address indexed to, uint256 value):
// 0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef.
var TransferTopic = Hash(ethaddr.Keccak256([]byte("Transfer(address,address,uint256)")))

// ERC-20 function selectors (first four bytes of the Keccak-256 of the
// signature).
var (
	selectorDecimals  = selector("decimals()")         // 0x313ce567
	selectorSymbol    = selector("symbol()")           // 0x95d89b41
	selectorBalanceOf = selector("balanceOf(address)") // 0x70a08231
)

func selector(signature string) []byte {
	sum := ethaddr.Keccak256([]byte(signature))
	return sum[:4]
}

// Errors returned by DecodeTransfer.
var (
	// ErrNotTransfer means the log is not a well-formed ERC-20 Transfer
	// event: wrong topic0, a topic count other than 3 (ERC-721 uses the
	// same topic0 with 4 topics), data that is not exactly one 32-byte
	// word, or address topics with non-zero padding.
	ErrNotTransfer = errors.New("evm: log is not an ERC-20 Transfer event")
	// ErrRemovedLog means the node flagged the log as removed by a
	// reorganisation; it does not describe a transfer on the canonical
	// chain.
	ErrRemovedLog = errors.New("evm: log was removed by a chain reorganisation")
)

// DecodeTransfer strictly decodes an ERC-20 Transfer event.
//
// It requires topic0 to be TransferTopic, exactly three topics, exactly 32
// bytes of data and properly padded address topics, and it refuses logs with
// the removed flag set. It does not look at which contract emitted the log:
// Transfer.Token carries that address and the caller must compare it with
// the token it expects.
func DecodeTransfer(l Log) (Transfer, error) {
	if l.Removed {
		return Transfer{}, ErrRemovedLog
	}
	if len(l.Topics) != 3 || l.Topics[0] != TransferTopic || len(l.Data) != 32 {
		return Transfer{}, ErrNotTransfer
	}
	from, err := topicAddress(l.Topics[1])
	if err != nil {
		return Transfer{}, ErrNotTransfer
	}
	to, err := topicAddress(l.Topics[2])
	if err != nil {
		return Transfer{}, ErrNotTransfer
	}
	return Transfer{
		Token:       l.Address,
		From:        from,
		To:          to,
		Amount:      new(big.Int).SetBytes(l.Data),
		TxHash:      l.TxHash,
		LogIndex:    l.LogIndex,
		BlockNumber: l.BlockNumber,
		BlockHash:   l.BlockHash,
	}, nil
}

// Decimals calls decimals() on an ERC-20 token. Tokens differ — USDT has 6
// decimals on Ethereum and 18 on BNB Smart Chain — so the value must be read,
// never assumed.
func (e *Endpoint) Decimals(ctx context.Context, token ethaddr.Address) (uint8, error) {
	out, err := e.Call(ctx, token, selectorDecimals)
	if err != nil {
		return 0, err
	}
	if len(out) != 32 {
		return 0, e.badResponse("eth_call", fmt.Errorf("decimals() returned %d bytes, want 32", len(out)))
	}
	v := new(big.Int).SetBytes(out)
	if !v.IsUint64() || v.Uint64() > 255 {
		return 0, e.badResponse("eth_call", errors.New("decimals() returned a value above 255"))
	}
	return uint8(v.Uint64()), nil
}

// maxSymbolLen bounds token symbols; real ones are a handful of characters.
const maxSymbolLen = 32

// Symbol calls symbol() on an ERC-20 token. Both the standard ABI string
// encoding and the legacy bytes32 encoding are understood. The result is
// validated to be short, printable UTF-8, since it ends up in the UI.
func (e *Endpoint) Symbol(ctx context.Context, token ethaddr.Address) (string, error) {
	out, err := e.Call(ctx, token, selectorSymbol)
	if err != nil {
		return "", err
	}
	sym, err := decodeSymbol(out)
	if err != nil {
		return "", e.badResponse("eth_call", err)
	}
	return sym, nil
}

func decodeSymbol(out []byte) (string, error) {
	var raw []byte
	switch {
	case len(out) == 32:
		// bytes32, right-padded with zeros.
		end := len(out)
		for end > 0 && out[end-1] == 0 {
			end--
		}
		raw = out[:end]
	case len(out) >= 64 && len(out)%32 == 0:
		offset := new(big.Int).SetBytes(out[:32])
		if !offset.IsUint64() || offset.Uint64() != 32 {
			return "", errors.New("symbol() returned an unexpected string offset")
		}
		length := new(big.Int).SetBytes(out[32:64])
		if !length.IsUint64() || length.Uint64() > uint64(len(out)-64) {
			return "", errors.New("symbol() returned an invalid string length")
		}
		raw = out[64 : 64+length.Uint64()]
	default:
		return "", fmt.Errorf("symbol() returned %d bytes, which is not an ABI string", len(out))
	}
	if len(raw) == 0 || len(raw) > maxSymbolLen || !utf8.Valid(raw) {
		return "", errors.New("symbol() returned an empty, oversized or non-UTF-8 string")
	}
	for _, r := range string(raw) {
		if !unicode.IsPrint(r) {
			return "", errors.New("symbol() returned non-printable characters")
		}
	}
	return string(raw), nil
}

// BalanceOf calls balanceOf(owner) on an ERC-20 token and returns the balance
// in base units.
func (e *Endpoint) BalanceOf(ctx context.Context, token, owner ethaddr.Address) (*big.Int, error) {
	data := make([]byte, 0, 36)
	data = append(data, selectorBalanceOf...)
	topic := addressTopic(owner)
	data = append(data, topic[:]...)
	out, err := e.Call(ctx, token, data)
	if err != nil {
		return nil, err
	}
	if len(out) != 32 {
		return nil, e.badResponse("eth_call", fmt.Errorf("balanceOf() returned %d bytes, want 32", len(out)))
	}
	return new(big.Int).SetBytes(out), nil
}
