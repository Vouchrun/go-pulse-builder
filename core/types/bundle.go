package types

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
	"github.com/holiman/uint256"
)

// EmptyUUID is the zero UUID.
var EmptyUUID uuid.UUID

// LatestUuidBundle tracks the latest bundle submitted by a signing address
// for a target block.
type LatestUuidBundle struct {
	Uuid           uuid.UUID
	SigningAddress common.Address
	BundleHash     common.Hash
	BundleUUID     uuid.UUID
}

// MevBundle is a bundle of transactions to be executed atomically.
type MevBundle struct {
	Txs               Transactions
	BlockNumber       *big.Int
	Uuid              uuid.UUID
	SigningAddress    common.Address
	MinTimestamp      uint64
	MaxTimestamp      uint64
	RevertingTxHashes []common.Hash
	Hash              common.Hash
}

func (b *MevBundle) UniquePayload() []byte {
	var buf []byte
	buf = binary.AppendVarint(buf, b.BlockNumber.Int64())
	buf = append(buf, b.Hash[:]...)
	sort.Slice(b.RevertingTxHashes, func(i, j int) bool {
		return bytes.Compare(b.RevertingTxHashes[i][:], b.RevertingTxHashes[j][:]) <= 0
	})
	for _, txHash := range b.RevertingTxHashes {
		buf = append(buf, txHash[:]...)
	}
	return buf
}

func (b *MevBundle) ComputeUUID() uuid.UUID {
	return uuid.NewHash(sha256.New(), uuid.Nil, b.UniquePayload(), 5)
}

func (b *MevBundle) RevertingHash(hash common.Hash) bool {
	for _, revHash := range b.RevertingTxHashes {
		if revHash == hash {
			return true
		}
	}
	return false
}

// SimulatedBundle is the result of simulating a bundle.
type SimulatedBundle struct {
	MevGasPrice       *uint256.Int
	TotalEth          *uint256.Int
	EthSentToCoinbase *uint256.Int
	TotalGasUsed      uint64
	OriginalBundle    MevBundle
}

// TimestampedTxHashSet is a set of transaction hashes with an expiry TTL.
type TimestampedTxHashSet struct {
	lock       sync.RWMutex
	timestamps map[common.Hash]time.Time
	ttl        time.Duration
}

func NewExpiringTxHashSet(ttl time.Duration) *TimestampedTxHashSet {
	s := &TimestampedTxHashSet{
		timestamps: make(map[common.Hash]time.Time),
		ttl:        ttl,
	}

	return s
}

func (s *TimestampedTxHashSet) Add(hash common.Hash) {
	s.lock.Lock()
	defer s.lock.Unlock()

	_, ok := s.timestamps[hash]
	if !ok {
		s.timestamps[hash] = time.Now().Add(s.ttl)
	}
}

func (s *TimestampedTxHashSet) Contains(hash common.Hash) bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	_, ok := s.timestamps[hash]
	return ok
}

func (s *TimestampedTxHashSet) Remove(hash common.Hash) {
	s.lock.Lock()
	defer s.lock.Unlock()

	_, ok := s.timestamps[hash]
	if ok {
		delete(s.timestamps, hash)
	}
}

func (s *TimestampedTxHashSet) Prune() {
	s.lock.Lock()
	defer s.lock.Unlock()

	now := time.Now()
	for hash, ts := range s.timestamps {
		if ts.Before(now) {
			delete(s.timestamps, hash)
		}
	}
}
