// Copyright (c) 2018 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package state

import (
	"container/heap"
	"math/big"
	"sort"
	"strings"

	"github.com/golang/groupcache/lru"
	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-core/blockchain/action"
	"github.com/iotexproject/iotex-core/config"
	"github.com/iotexproject/iotex-core/iotxaddress"
	"github.com/iotexproject/iotex-core/logger"
	"github.com/iotexproject/iotex-core/pkg/hash"
	"github.com/iotexproject/iotex-core/trie"
)

const (
	// Level 1 is for candidate pool
	candidatePool = 1
	// Level 2 is for candidate buffer pool
	candidateBufferPool = candidatePool + 1
)

const candidateBufferSize = 100

var (
	// ErrInvalidAddr is the error that the address format is invalid, cannot be decoded
	ErrInvalidAddr = errors.New("address format is invalid")

	// ErrNotEnoughBalance is the error that the balance is not enough
	ErrNotEnoughBalance = errors.New("not enough balance")

	// ErrAccountNotExist is the error that the account does not exist
	ErrAccountNotExist = errors.New("the account does not exist")

	// ErrFailedToMarshalState is the error that the state marshaling is failed
	ErrFailedToMarshalState = errors.New("failed to marshal state")

	// ErrFailedToUnmarshalState is the error that the state un-marshaling is failed
	ErrFailedToUnmarshalState = errors.New("failed to unmarshal state")
)

type (
	// Factory defines an interface for managing states
	Factory interface {
		CreateState(string, uint64) (*State, error)
		Balance(string) (*big.Int, error)
		CommitStateChanges(uint64, []*action.Transfer, []*action.Vote) error
		// Note that nonce starts with 1.
		Nonce(string) (uint64, error)
		State(string) (*State, error)
		RootHash() hash.Hash32B
		Candidates() (uint64, []*Candidate)
		CandidatesByHeight(uint64) ([]*Candidate, bool)
	}

	// factory implements StateFactory interface, tracks changes in a map and batch-commits to trie/db
	factory struct {
		// candidate pool
		currentChainHeight     uint64
		candidatesLRU          *lru.Cache
		candidateHeap          CandidateMinPQ
		candidateBufferMinHeap CandidateMinPQ
		candidateBufferMaxHeap CandidateMaxPQ
		cachedCandidate        map[string]*Candidate
		// accounts
		cachedAccount map[string]*State // accounts being modified in this Tx
		trie          trie.Trie         // global state trie
	}
)

// FactoryOption sets Factory construction parameter
type FactoryOption func(*factory, *config.Config) error

// PrecreatedTrieOption uses pre-created trie for state factory
func PrecreatedTrieOption(tr trie.Trie) FactoryOption {
	return func(sf *factory, cfg *config.Config) error {
		sf.trie = tr

		return nil
	}
}

// DefaultTrieOption creates trie from config for state factory
func DefaultTrieOption() FactoryOption {
	return func(sf *factory, cfg *config.Config) error {
		dbPath := cfg.Chain.TrieDBPath
		if len(dbPath) == 0 {
			return errors.New("Invalid empty trie db path")
		}
		tr, err := trie.NewTrie(dbPath, trie.AccountKVNameSpace, trie.EmptyRoot, false)
		if err != nil {
			return errors.Wrapf(err, "Failed to generate trie from config")
		}
		sf.trie = tr

		return nil
	}
}

// InMemTrieOption creates in memory trie for state factory
func InMemTrieOption() FactoryOption {
	return func(sf *factory, cfg *config.Config) error {
		tr, err := trie.NewTrie("", trie.AccountKVNameSpace, trie.EmptyRoot, true)
		if err != nil {
			return errors.Wrapf(err, "Failed to initialize in-memory trie")
		}
		sf.trie = tr

		return nil
	}
}

// NewFactory creates a new state factory
func NewFactory(cfg *config.Config, opts ...FactoryOption) (Factory, error) {
	sf := &factory{
		currentChainHeight:     0,
		candidatesLRU:          lru.New(int(cfg.Chain.DelegateLRUSize)),
		candidateHeap:          CandidateMinPQ{int(cfg.Chain.NumCandidates), make([]*Candidate, 0)},
		candidateBufferMinHeap: CandidateMinPQ{candidateBufferSize, make([]*Candidate, 0)},
		candidateBufferMaxHeap: CandidateMaxPQ{candidateBufferSize, make([]*Candidate, 0)},
		cachedCandidate:        make(map[string]*Candidate),
		cachedAccount:          make(map[string]*State),
	}

	for _, opt := range opts {
		if err := opt(sf, cfg); err != nil {
			logger.Error().Err(err).Msgf("Failed to create state factory option %s", opt)
			return nil, err
		}
	}
	return sf, nil
}

// CreateState adds a new State with initial balance to the factory
func (sf *factory) CreateState(addr string, init uint64) (*State, error) {
	pubKeyHash := iotxaddress.GetPubkeyHash(addr)
	if pubKeyHash == nil {
		return nil, ErrInvalidAddr
	}
	balance := big.NewInt(0)
	weight := big.NewInt(0)
	balance.SetUint64(init)
	s := State{Balance: balance, VotingWeight: weight}
	mstate, err := stateToBytes(&s)
	if err != nil {
		return nil, err
	}
	if err := sf.trie.Upsert(pubKeyHash, mstate); err != nil {
		return nil, err
	}
	return &s, nil
}

// Balance returns balance
func (sf *factory) Balance(addr string) (*big.Int, error) {
	state, err := sf.getState(addr)
	if err != nil {
		return nil, err
	}
	return state.Balance, nil
}

// Nonce returns the nonce if the account exists
func (sf *factory) Nonce(addr string) (uint64, error) {
	state, err := sf.getState(addr)
	if err != nil {
		return 0, err
	}
	return state.Nonce, nil
}

// State returns the state if the address exists
func (sf *factory) State(addr string) (*State, error) {
	return sf.getState(addr)
}

// RootHash returns the hash of the root node of the trie
func (sf *factory) RootHash() hash.Hash32B {
	return sf.trie.RootHash()
}

// CommitStateChanges updates a State from the given actions
func (sf *factory) CommitStateChanges(blockHeight uint64, tsf []*action.Transfer, vote []*action.Vote) error {
	if err := sf.handleTsf(tsf); err != nil {
		return err
	}
	if err := sf.handleVote(blockHeight, vote); err != nil {
		return err
	}

	// construct <k, v> list of pending state
	transferK := [][]byte{}
	transferV := [][]byte{}
	for address, state := range sf.cachedAccount {
		ss, err := stateToBytes(state)
		if err != nil {
			return err
		}
		pkhash := iotxaddress.GetPubkeyHash(address)
		addr := make([]byte, len(pkhash))
		copy(addr, pkhash[:])
		transferK = append(transferK, addr)
		transferV = append(transferV, ss)

		// Perform vote update operation on candidate and delegate pools
		if !state.IsCandidate {
			// remove the candidate if the person is not a candidate anymore
			if _, ok := sf.cachedCandidate[address]; ok {
				delete(sf.cachedCandidate, address)
			}
			sf.removeCandidate(address)
			continue
		}
		totalWeight := big.NewInt(0)
		totalWeight.Add(totalWeight, state.VotingWeight)
		if state.Votee == address {
			totalWeight.Add(totalWeight, state.Balance)
		}
		sf.updateCandidate(address, totalWeight, blockHeight)
	}
	sf.currentChainHeight = blockHeight
	candidates := sf.candidateHeap.CandidateList()
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Votes.Cmp(candidates[j].Votes) == 0 {
			return strings.Compare(candidates[i].Address, candidates[j].Address) < 0
		}
		return candidates[i].Votes.Cmp(candidates[j].Votes) < 0
	})
	sf.candidatesLRU.Add(sf.currentChainHeight, candidates)

	// commit the state changes to Trie in a batch
	return sf.trie.Commit(transferK, transferV)
}

// Candidates returns array of candidates in candidate pool
func (sf *factory) Candidates() (uint64, []*Candidate) {
	return sf.currentChainHeight, sf.candidateHeap.CandidateList()
}

// CandidatesByHeight returns array of candidates in candidate pool of a given height
func (sf *factory) CandidatesByHeight(height uint64) ([]*Candidate, bool) {
	if candidates, ok := sf.candidatesLRU.Get(height); ok {
		return candidates.([]*Candidate), ok
	}
	return []*Candidate{}, false
}

//======================================
// private functions
//=====================================
func (sf *factory) candidatesBuffer() (uint64, []*Candidate) {
	return sf.currentChainHeight, sf.candidateBufferMinHeap.CandidateList()
}

// getState pulls an existing State
func (sf *factory) getState(addr string) (*State, error) {
	pubKeyHash := iotxaddress.GetPubkeyHash(addr)
	return sf.getStateFromPKHash(pubKeyHash)
}

func (sf *factory) getStateFromPKHash(pubKeyHash []byte) (*State, error) {
	if pubKeyHash == nil {
		return nil, ErrInvalidAddr
	}
	mstate, err := sf.trie.Get(pubKeyHash)
	if errors.Cause(err) == trie.ErrNotExist {
		return nil, ErrAccountNotExist
	}
	if err != nil {
		return nil, err
	}
	return bytesToState(mstate)
}

func (sf *factory) updateCandidate(address string, totalWeight *big.Int, blockHeight uint64) {
	// Candidate was added when self-nomination, always exist in cached candidate
	candidate, _ := sf.cachedCandidate[address]
	candidate.Votes = totalWeight
	candidate.LastUpdateHeight = blockHeight
	_, level := sf.inPool(candidate.Address)
	switch level {
	case candidatePool:
		// if candidate is already in candidate pool
		sf.candidateHeap.update(candidate, candidate.Votes)
	case candidateBufferPool:
		// if candidate is already in candidate buffer pool
		sf.candidateBufferMinHeap.update(candidate, candidate.Votes)
		sf.candidateBufferMaxHeap.update(candidate, candidate.Votes)
	default:
		// candidate is not in any of two pools
		transitCandidate := candidate
		if sf.candidateHeap.shouldTake(transitCandidate.Votes) {
			// Push candidate into candidate pool
			heap.Push(&sf.candidateHeap, transitCandidate)
			transitCandidate = nil
			if sf.candidateHeap.Len() > sf.candidateHeap.Capacity {
				transitCandidate = heap.Pop(&sf.candidateHeap).(*Candidate)
			}
		}
		if transitCandidate != nil && sf.candidateBufferMinHeap.shouldTake(transitCandidate.Votes) {
			// Push candidate into candidate pool
			heap.Push(&sf.candidateBufferMinHeap, transitCandidate)
			heap.Push(&sf.candidateBufferMaxHeap, transitCandidate)
			transitCandidate = nil
			if sf.candidateBufferMinHeap.Len() > sf.candidateBufferMinHeap.Capacity {
				transitCandidate = heap.Pop(&sf.candidateBufferMinHeap).(*Candidate)
				heap.Remove(&sf.candidateBufferMaxHeap, transitCandidate.maxIndex)
			}
		}
	}
	sf.balance()

	// Temporarily leave it here to check the algorithm is correct
	if sf.candidateBufferMinHeap.Len() != sf.candidateBufferMaxHeap.Len() {
		logger.Warn().Msg("candidateBuffer min and max heap not sync")
	}
}

func (sf *factory) removeCandidate(address string) {
	c, level := sf.inPool(address)
	switch level {
	case candidatePool:
		heap.Remove(&sf.candidateHeap, c.minIndex)
		if sf.candidateBufferMinHeap.Len() > 0 {
			promoteCandidate := heap.Pop(&sf.candidateBufferMaxHeap).(*Candidate)
			heap.Remove(&sf.candidateBufferMinHeap, promoteCandidate.minIndex)
			heap.Push(&sf.candidateHeap, promoteCandidate)
		}
	case candidateBufferPool:
		heap.Remove(&sf.candidateBufferMinHeap, c.minIndex)
		heap.Remove(&sf.candidateBufferMaxHeap, c.maxIndex)
	default:
		break
	}
	sf.balance()

	// Temporarily leave it here to check the algorithm is correct
	if sf.candidateBufferMinHeap.Len() != sf.candidateBufferMaxHeap.Len() {
		logger.Warn().Msg("candidateBuffer min and max heap not sync")
	}
}

func (sf *factory) balance() {
	if sf.candidateHeap.Len() > 0 && sf.candidateBufferMaxHeap.Len() > 0 && sf.candidateHeap.Top().(*Candidate).Votes.Cmp(sf.candidateBufferMaxHeap.Top().(*Candidate).Votes) < 0 {
		cFromCandidatePool := heap.Pop(&sf.candidateHeap).(*Candidate)
		cFromCandidateBufferPool := heap.Pop(&sf.candidateBufferMaxHeap).(*Candidate)
		heap.Remove(&sf.candidateBufferMinHeap, cFromCandidateBufferPool.minIndex)
		heap.Push(&sf.candidateHeap, cFromCandidateBufferPool)
		heap.Push(&sf.candidateBufferMinHeap, cFromCandidatePool)
		heap.Push(&sf.candidateBufferMaxHeap, cFromCandidatePool)
	}
}

func (sf *factory) inPool(address string) (*Candidate, int) {
	if c := sf.candidateHeap.exist(address); c != nil {
		return c, candidatePool // The candidate exists in the Candidate pool
	}
	if c := sf.candidateBufferMinHeap.exist(address); c != nil {
		return c, candidateBufferPool // The candidate exists in the Candidate buffer pool
	}
	return nil, 0
}

func (sf *factory) cache(address string) (*State, error) {
	if state, exist := sf.cachedAccount[address]; exist {
		return state, nil
	}
	state, err := sf.getState(address)
	switch {
	case err == ErrAccountNotExist:
		if state, err = sf.CreateState(address, 0); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	}
	sf.cachedAccount[address] = state
	return state, nil
}

func (sf *factory) handleTsf(tsf []*action.Transfer) error {
	for _, tx := range tsf {
		if !tx.IsCoinbase {
			// check sender
			sender, err := sf.cache(tx.Sender)
			if err != nil {
				return err
			}
			if tx.Amount.Cmp(sender.Balance) == 1 {
				return ErrNotEnoughBalance
			}
			// update sender balance
			if err := sender.SubBalance(tx.Amount); err != nil {
				return err
			}
			// update sender nonce
			if tx.Nonce > sender.Nonce {
				sender.Nonce = tx.Nonce
			}
			// Update sender votes
			if len(sender.Votee) > 0 && sender.Votee != tx.Sender {
				// sender already voted to a different person
				voteeOfSender, err := sf.cache(sender.Votee)
				if err != nil {
					return err
				}
				voteeOfSender.VotingWeight.Sub(voteeOfSender.VotingWeight, tx.Amount)
			}
		}
		// check recipient
		recipient, err := sf.cache(tx.Recipient)
		if err != nil {
			return err
		}
		// update recipient balance
		if err := recipient.AddBalance(tx.Amount); err != nil {
			return err
		}
		// Update recipient votes
		if len(recipient.Votee) > 0 && recipient.Votee != tx.Recipient {
			// recipient already voted to a different person
			voteeOfRecipient, err := sf.cache(recipient.Votee)
			if err != nil {
				return err
			}
			voteeOfRecipient.VotingWeight.Add(voteeOfRecipient.VotingWeight, tx.Amount)
		}
	}
	return nil
}

func (sf *factory) handleVote(blockHeight uint64, vote []*action.Vote) error {
	for _, v := range vote {
		voterAddress := v.VoterAddress
		voteFrom, err := sf.cache(voterAddress)
		if err != nil {
			return err
		}

		// update voteFrom nonce
		if v.Nonce > voteFrom.Nonce {
			voteFrom.Nonce = v.Nonce
		}
		// Update old votee's weight
		if len(voteFrom.Votee) > 0 && voteFrom.Votee != voterAddress {
			// voter already voted
			oldVotee, err := sf.cache(voteFrom.Votee)
			if err != nil {
				return err
			}
			oldVotee.VotingWeight.Sub(oldVotee.VotingWeight, voteFrom.Balance)
			voteFrom.Votee = ""
		}

		voteeAddress := v.VoteeAddress
		if voteeAddress == "" {
			// unvote operation
			voteFrom.IsCandidate = false
			continue
		}

		voteTo, err := sf.cache(voteeAddress)
		if err != nil {
			return err
		}

		if voterAddress != voteeAddress {
			// Voter votes to a different person
			voteTo.VotingWeight.Add(voteTo.VotingWeight, voteFrom.Balance)
			voteFrom.Votee = voteeAddress
		} else {
			// Vote to self: self-nomination or cancel the previous vote case
			voteFrom.Votee = voterAddress
			voteFrom.IsCandidate = true
			if _, ok := sf.cachedCandidate[voterAddress]; !ok {
				sf.cachedCandidate[voterAddress] = &Candidate{
					Address:        voterAddress,
					PubKey:         v.SelfPubkey[:],
					CreationHeight: blockHeight,
					minIndex:       0,
					maxIndex:       0,
				}
			}
		}
	}
	return nil
}
