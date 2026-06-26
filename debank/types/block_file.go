package types

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
)

type BlockFile struct {
	Block            Block         `json:"block"`
	Txs              []Transaction `json:"txs"`
	Events           []Event       `json:"events"`
	Traces           []Trace       `json:"traces"`
	ErrorEvents      []Event       `json:"error_events"`
	ErrorTraces      []Trace       `json:"error_traces"`
	StorageContracts []string      `json:"storage_contracts"`
}

func (bf *BlockFile) Validation() BlockValidation {
	var ids []string

	// Collect all IDs
	ids = append(ids, bf.Block.ID) // assuming Block has an ID field
	for _, tx := range bf.Txs {
		ids = append(ids, tx.ID)
	}
	for _, event := range bf.Events {
		ids = append(ids, event.ID)
	}
	for _, trace := range bf.Traces {
		ids = append(ids, trace.ID)
	}

	return BlockValidation{ValidationHash: CalcValidationHash(ids), IsFork: false}
}

func CalcValidationHash(ids []string) int64 {
	sha1Sum := big.NewInt(0)
	for _, each := range ids {
		h := sha1.New()
		h.Write([]byte(each))
		hash := hex.EncodeToString(h.Sum(nil))
		hashInt := new(big.Int)
		_, ok := hashInt.SetString(hash, 16)
		if !ok {
			panic(fmt.Sprintf("Failed to convert id %s to %s to big.Int", each, hash))
		}
		sha1Sum.Add(sha1Sum, hashInt)
	}

	sha1SumStr := sha1Sum.String()
	last8Digits := sha1SumStr[len(sha1SumStr)-6:]
	validationHash, _ := strconv.ParseInt(last8Digits, 10, 64)
	return validationHash
}

// BlockValidation  { 'validation_hash': 12345678, is_fork: false }
type BlockValidation struct {
	ValidationHash int64 `json:"validation_hash"`
	IsFork         bool  `json:"is_fork"`
}

type DebankOutPut struct {
	BlockFile      *BlockFile        `json:"block_file"`
	Header         *Header           `json:"header"`
	StateDiff      *BlockStorageDiff `json:"state_diff"`
	ValidationHash int64             `json:"validation_hash"`
}
