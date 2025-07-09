// Package poi implements the Proof-of-Intelligence consensus engine.
package poi

import (
	"errors"
	"fmt"
	"math/rand"
	"math/big"
	"sort"
	"sync"
	"time"
	"hash"
	"context"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/consensus"
	"golang.org/x/crypto/sha3"
	"github.com/ethereum/go-ethereum/crypto"
)

type ValidatorState struct {
	BlocksProduced     uint64
	ConsecutiveBlocks  uint64
	CooldownUntilBlock uint64
	LastSeenBlock      uint64
	TotalUptime        time.Duration
	TotalActiveTime    time.Duration
	TotalTxProcessed   uint64
	SuccessfulTx       uint64
}

// FIXED: Use a consistent identifier that doesn't change with signing
type SealTask struct {
	BlockNumber   uint64
	UnsignedHash  common.Hash  // Hash of header without signature - this stays constant
	SignedHash    common.Hash  // Hash of header with signature - for final verification
	Block         *types.Block
	Results       chan<- *types.Block
	Cancel        context.CancelFunc
	StartTime     time.Time
}

type PoIEngine struct {
	db         ethdb.Database
	signer     common.Address
	signFn     func(accounts.Account, string, []byte) ([]byte, error)

	validatorStates map[common.Address]*ValidatorState
	repStore    map[common.Address]float64
	perfStore   map[common.Address]float64
	blockNumber uint64
	config *params.PoIConfig

	// FIXED: Use unsigned hash as the primary key since it's stable
	activeTasks         map[uint64]*SealTask                    // Track by block number for cleanup
	activeTasksByUnsigned map[common.Hash]*SealTask            // Track by unsigned hash for mining lookup
	taskMutex           sync.RWMutex

	lock sync.RWMutex
}

func New(chainConfig *params.ChainConfig, db ethdb.Database) *PoIEngine {
	config := chainConfig.PoI
	if config == nil {
		// Set default config if not provided
		config = &params.PoIConfig{
			Period:                2,  // 2 second block time
			Alpha:                 0.7,
			Beta:                  0.3,
			BoostFactor:          1.2,
			SlidingWindowPercent: 0.3,
			CooldownTrigger:      5,
			CooldownPeriod:       10,
			DecayEpochSize:       100,
		}
	}
	
	return &PoIEngine{
		config:                config,
		db:                    db,
		validatorStates:       make(map[common.Address]*ValidatorState),
		repStore:              make(map[common.Address]float64),
		perfStore:             make(map[common.Address]float64),
		activeTasks:           make(map[uint64]*SealTask),
		activeTasksByUnsigned: make(map[common.Hash]*SealTask),
	}
}

func (e *PoIEngine) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	e.lock.Lock()
	defer e.lock.Unlock()

	e.blockNumber = header.Number.Uint64()

	validators := make([]common.Address, 0, len(e.validatorStates))
	for addr := range e.validatorStates {
		validators = append(validators, addr)
	}

	chosen := e.selectValidator(validators)
	if chosen == (common.Address{}) {
		return errors.New("no validator available for block production")
	}
	
	header.Coinbase = chosen

	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return fmt.Errorf("parent block not found for block %d", header.Number.Uint64())
	}
	
	parentTime := parent.Time
	now := uint64(time.Now().Unix())
	
	// Ensure minimum block time
	minTime := parentTime + uint64(e.config.Period)
	if now < minTime {
		now = minTime
	}
	
	header.Time = now
	header.Nonce = types.BlockNonce{}
	header.Difficulty = big.NewInt(1)
	header.GasLimit = parent.GasLimit

	// Ensure Extra field has enough space for signature
	if len(header.Extra) < 65 {
		extra := make([]byte, 65)
		copy(extra, header.Extra)
		header.Extra = extra
	}

	log.Info("Prepared header with validator", "validator", chosen.Hex(), "time", header.Time, "parent_time", parentTime)
	return nil
}

func (e *PoIEngine) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, withdrawals []*types.Withdrawal) {
	e.lock.Lock()
	defer e.lock.Unlock()

	// FIXED: Only commit state if not already committed
	if header.Root == (common.Hash{}) && state != nil {
		stateRoot, err := state.Commit(header.Number.Uint64(), true, false)
		if err != nil {
			log.Error("Failed to commit state in Finalize", "err", err, "number", header.Number)
		} else {
			header.Root = stateRoot
			log.Debug("Committed state root in Finalize", "root", stateRoot.Hex(), "number", header.Number)
		}
	}

	// Update validator state after successful block
	validator := header.Coinbase
	e.updateValidatorState(validator)
	
	if e.config.DecayEpochSize > 0 && e.blockNumber%e.config.DecayEpochSize == 0 {
		e.decayAllReputation()
	}

	log.Debug("Finalized block", "number", header.Number, "validator", validator.Hex(), "txs", len(txs))
}

func (e *PoIEngine) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt, withdrawals []*types.Withdrawal) (*types.Block, error) {
	e.lock.Lock()
	defer e.lock.Unlock()

	// Update validator state after successful block
	validator := header.Coinbase
	e.updateValidatorState(validator)
	
	if e.config.DecayEpochSize > 0 && e.blockNumber%e.config.DecayEpochSize == 0 {
		e.decayAllReputation()
	}

	// FIXED: Proper state root handling
	if header.Root == (common.Hash{}) {
		if state != nil {
			stateRoot, err := state.Commit(header.Number.Uint64(), true, false)
			if err != nil {
				return nil, fmt.Errorf("failed to commit state: %v", err)
			}
			header.Root = stateRoot
			log.Debug("Committed state root in FinalizeAndAssemble", "root", stateRoot.Hex(), "number", header.Number)
		} else {
			return nil, errors.New("state is nil and state root is zero")
		}
	}

	// Calculate receipts root
	if len(receipts) > 0 {
		header.ReceiptHash = types.DeriveSha(types.Receipts(receipts), trie.NewStackTrie(nil))
	} else {
		header.ReceiptHash = types.EmptyReceiptsHash
	}

	// Calculate transaction root
	if len(txs) > 0 {
		header.TxHash = types.DeriveSha(types.Transactions(txs), trie.NewStackTrie(nil))
	} else {
		header.TxHash = types.EmptyTxsHash
	}

	header.UncleHash = types.EmptyUncleHash

	log.Debug("Assembled block", "number", header.Number, "validator", validator.Hex(), "txs", len(txs), "state_root", header.Root.Hex())
	body := &types.Body{
    Transactions: txs,
    Uncles:       uncles,
}
hasher := trie.NewStackTrieHasher(16)
return types.NewBlock(header, body, receipts, hasher), nil
}

func (e *PoIEngine) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	e.lock.RLock()
	defer e.lock.RUnlock()

	if header.Number == nil {
		return errors.New("header number is nil")
	}
	if header.Difficulty == nil {
		return errors.New("header difficulty is nil")
	}
	if header.Difficulty.Cmp(big.NewInt(1)) != 0 {
		return fmt.Errorf("invalid difficulty: got %v, want 1", header.Difficulty)
	}
	if header.GasLimit == 0 {
		return errors.New("header gas limit is zero")
	}

	signer := header.Coinbase
	if signer == (common.Address{}) {
		return errors.New("header coinbase is empty")
	}

	// Verify signature if not genesis block
	if header.Number.Uint64() > 0 {
		if err := e.verifySeal(header); err != nil {
			return fmt.Errorf("invalid block signature: %v", err)
		}
	}

	// Skip validator checks for genesis or admin
	if header.Number.Uint64() == 0 || signer == e.signer {
		return nil
	}

	state, ok := e.validatorStates[signer]
	if !ok {
		return fmt.Errorf("unknown validator %s", signer.Hex())
	}
	
	if e.blockNumber < state.CooldownUntilBlock {
		return fmt.Errorf("validator %s is in cooldown", signer.Hex())
	}
	
	return nil
}

func (e *PoIEngine) verifySeal(header *types.Header) error {
	if len(header.Extra) < 65 {
		return errors.New("extra field too short for signature")
	}

	signature := header.Extra[len(header.Extra)-65:]
	sealHash := e.SealHash(header)
	
	pubkey, err := crypto.Ecrecover(sealHash.Bytes(), signature)
	if err != nil {
		return fmt.Errorf("failed to recover public key: %v", err)
	}
	
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])
	
	if signer != header.Coinbase {
		return fmt.Errorf("invalid signature: expected %s, got %s", header.Coinbase.Hex(), signer.Hex())
	}
	
	return nil
}

func (e *PoIEngine) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	go func() {
		defer close(results)
		for _, header := range headers {
			select {
			case <-abort:
				return
			default:
				err := e.VerifyHeader(chain, header)
				results <- err
			}
		}
	}()

	return abort, results
}

func (e *PoIEngine) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errors.New("uncle blocks are not allowed in PoI")
	}
	return nil
}

// FIXED: Completely redesigned Seal method with proper hash consistency
func (e *PoIEngine) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	e.lock.RLock()
	signer, signFn := e.signer, e.signFn
	e.lock.RUnlock()

	if signer == (common.Address{}) || signFn == nil {
		return errors.New("PoI: signer not set or signFn nil")
	}

	header := block.Header()
	number := header.Number.Uint64()
	if number == 0 {
		return errors.New("PoI: cannot seal genesis block")
	}

	// Ensure Extra field has proper size
	if len(header.Extra) < 65 {
		extra := make([]byte, 65)
		copy(extra, header.Extra)
		header.Extra = extra
	}

	// FIXED: Calculate unsigned seal hash - this will remain constant throughout the process
	unsignedSealHash := e.SealHash(header)

	// FIXED: Use unsigned hash as the stable identifier
	e.taskMutex.Lock()
	
	// Cancel any existing task for this block number
	if existingTask, exists := e.activeTasks[number]; exists {
		existingTask.Cancel()
		delete(e.activeTasks, number)
		// Remove from unsigned hash map
		if existingTask.UnsignedHash != (common.Hash{}) {
			delete(e.activeTasksByUnsigned, existingTask.UnsignedHash)
		}
	}

	// Create context for this sealing task
	ctx, cancel := context.WithCancel(context.Background())
	task := &SealTask{
		BlockNumber:  number,
		UnsignedHash: unsignedSealHash, // This stays constant
		SignedHash:   common.Hash{},    // Will be filled after signing
		Block:        block,
		Results:      results,
		Cancel:       cancel,
		StartTime:    time.Now(),
	}
	
	// Store task by both block number and unsigned hash
	e.activeTasks[number] = task
	e.activeTasksByUnsigned[unsignedSealHash] = task
	e.taskMutex.Unlock()

	log.Info("Starting seal task", "number", number, "unsigned_hash", unsignedSealHash.Hex()[:8], "signer", signer.Hex())

	go func() {
		defer func() {
			// Cleanup task when done
			e.taskMutex.Lock()
			delete(e.activeTasks, number)
			delete(e.activeTasksByUnsigned, unsignedSealHash)
			e.taskMutex.Unlock()
		}()

		// Calculate delay based on header time
		delay := time.Until(time.Unix(int64(header.Time), 0))
		if delay < 0 {
			delay = 0
		}

		// Wait for the appropriate time or stop signal
		select {
		case <-stop:
			log.Debug("Seal stopped by external signal", "number", number)
			return
		case <-ctx.Done():
			log.Debug("Seal cancelled by context", "number", number)
			return
		case <-time.After(delay):
			// Continue with sealing
		}

		// Double check we're not stopped
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		default:
		}

		// Create header copy for signing
		signedHeader := *header
		
		// Sign the block using the unsigned seal hash
		sig, err := signFn(accounts.Account{Address: signer}, "poi/seal", unsignedSealHash.Bytes())
		if err != nil {
			log.Error("Failed to sign block", "number", number, "err", err)
			return
		}

		// Inject signature
		copy(signedHeader.Extra[len(signedHeader.Extra)-65:], sig)

		// FIXED: Calculate and store the signed hash, but keep using unsigned hash as identifier
		signedSealHash := e.SealHash(&signedHeader)
		
		// Update task with signed hash
		e.taskMutex.Lock()
		if currentTask, exists := e.activeTasks[number]; exists {
			currentTask.SignedHash = signedSealHash
		}
		e.taskMutex.Unlock()

		// Create sealed block
		body := &types.Body{
    Transactions: block.Transactions(),
    Uncles:       block.Uncles(),
}
		sealed := types.NewBlock(&signedHeader, body, nil, types.HomesteadTrieHasher{})
		// Send result
		select {
		case results <- sealed:
			log.Info("Block sealed successfully", "number", number, "hash", sealed.Hash().Hex(), 
				"unsigned_hash", unsignedSealHash.Hex()[:8], "signed_hash", signedSealHash.Hex()[:8], "signer", signer.Hex())
		case <-stop:
			log.Debug("Seal result not sent due to stop", "number", number)
		case <-ctx.Done():
			log.Debug("Seal result not sent due to cancellation", "number", number)
		default:
			log.Warn("Seal result channel blocked", "number", number)
		}
	}()

	return nil
}

// FIXED: Update method to use unsigned hash for lookup
func (e *PoIEngine) GetSealTask(sealHash common.Hash) *SealTask {
	e.taskMutex.RLock()
	defer e.taskMutex.RUnlock()
	
	// First try to find by unsigned hash (this should be the stable identifier)
	if task := e.activeTasksByUnsigned[sealHash]; task != nil {
		return task
	}
	
	// Fallback: check if this is a signed hash matching any task
	for _, task := range e.activeTasks {
		if task.SignedHash == sealHash {
			return task
		}
	}
	
	return nil
}

// FIXED: Add method to get task by unsigned hash specifically
func (e *PoIEngine) GetSealTaskByUnsigned(unsignedHash common.Hash) *SealTask {
	e.taskMutex.RLock()
	defer e.taskMutex.RUnlock()
	return e.activeTasksByUnsigned[unsignedHash]
}

func (e *PoIEngine) SealHash(header *types.Header) common.Hash {
	return computeSealHash(header)
}

func computeSealHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()
	encodeHeaderWithoutSignature(hasher, header)
	hasher.Sum(hash[:0])
	return hash
}

func encodeHeaderWithoutSignature(hasher hash.Hash, header *types.Header) {
	// Create header copy
	copyHeader := *header
	
	// Create copy of Extra field and clear signature part
	if len(header.Extra) >= 65 {
		extra := make([]byte, len(header.Extra))
		copy(extra, header.Extra)
		// Zero out last 65 bytes (signature part)
		for i := len(extra) - 65; i < len(extra); i++ {
			extra[i] = 0
		}
		copyHeader.Extra = extra
	}
	
	rlp.Encode(hasher, &copyHeader)
}

func (e *PoIEngine) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return []rpc.API{
		{
			Namespace: "poi",
			Service:   &PoIAPI{engine: e},
			Public:    true,
		},
	}
}

type PoIAPI struct {
	engine *PoIEngine
}

func (api *PoIAPI) GetReputation(addr common.Address) float64 {
	return api.engine.getReputation(addr)
}

func (api *PoIAPI) GetPerformance(addr common.Address) float64 {
	return api.engine.getPerformance(addr)
}

func (api *PoIAPI) GetValidatorState(addr common.Address) *ValidatorState {
	return api.engine.validatorStates[addr]
}

func (e *PoIEngine) selectValidator(validators []common.Address) common.Address {
	candidates := make([]common.Address, 0)
	for _, v := range validators {
		state := e.validatorStates[v]
		if state == nil || e.blockNumber >= state.CooldownUntilBlock {
			candidates = append(candidates, v)
		}
	}
	
	// If no validators available, use admin node
	if len(candidates) == 0 {
		if e.signer != (common.Address{}) {
			log.Debug("Using admin node for sealing", "admin", e.signer.Hex())
			// Add admin to validator states if not present
			if e.validatorStates[e.signer] == nil {
				e.validatorStates[e.signer] = &ValidatorState{}
			}
			return e.signer
		}
		log.Error("No validator available including admin node")
		return common.Address{}
	}
	
	if len(candidates) == 1 {
		return candidates[0]
	}
	
	scores := make(map[common.Address]float64)
	for _, addr := range candidates {
		rep := e.getReputation(addr)
		perf := e.getPerformance(addr)
		scores[addr] = e.config.Alpha*rep + e.config.Beta*perf
	}
	top := e.selectTopPercent(scores, e.config.SlidingWindowPercent)
	chosen := top[rand.Intn(len(top))]
	
	return chosen
}

func (e *PoIEngine) getReputation(addr common.Address) float64 {
	state := e.validatorStates[addr]
	if state == nil {
		return 0.5 * e.config.BoostFactor
	}

	var blockScore float64 = 0.0
	if e.blockNumber > 0 {
		blockScore = float64(state.BlocksProduced) / float64(e.blockNumber)
	}

	uptimeScore := 0.0
	if state.TotalActiveTime > 0 {
		uptimeScore = state.TotalUptime.Seconds() / state.TotalActiveTime.Seconds()
	}
	txSuccessRate := 0.0
	if state.TotalTxProcessed > 0 {
		txSuccessRate = float64(state.SuccessfulTx) / float64(state.TotalTxProcessed)
	}

	rep := 0.4*blockScore + 0.3*uptimeScore + 0.3*txSuccessRate

	if e.config.DecayEpochSize > 0 && state.BlocksProduced < e.config.DecayEpochSize*3 {
		rep *= e.config.BoostFactor
	}

	return rep
}

func (e *PoIEngine) getPerformance(addr common.Address) float64 {
	return e.perfStore[addr]
}

func (e *PoIEngine) updateValidatorState(addr common.Address) {
	state := e.validatorStates[addr]
	if state == nil {
		state = &ValidatorState{}
		e.validatorStates[addr] = state
	}
	state.BlocksProduced++
	state.ConsecutiveBlocks++
	state.LastSeenBlock = e.blockNumber
	if state.ConsecutiveBlocks >= e.config.CooldownTrigger {
		state.CooldownUntilBlock = e.blockNumber + e.config.CooldownPeriod
		state.ConsecutiveBlocks = 0
	}
}

func (e *PoIEngine) decayAllReputation() {
	for addr, score := range e.repStore {
		e.repStore[addr] = score * 0.7
	}
}

func (e *PoIEngine) selectTopPercent(scores map[common.Address]float64, percent float64) []common.Address {
	type kv struct {
		Key   common.Address
		Value float64
	}
	var sorted []kv
	for k, v := range scores {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Value > sorted[j].Value
	})

	n := int(float64(len(sorted)) * percent)
	if n == 0 && len(sorted) > 0 {
		n = 1
	}
	result := make([]common.Address, 0, n)
	for i := 0; i < n; i++ {
		result = append(result, sorted[i].Key)
	}
	return result
}

func (e *PoIEngine) Authorize(signer common.Address, signFn func(accounts.Account, string, []byte) ([]byte, error)) {
	e.lock.Lock()
	defer e.lock.Unlock()
	e.signer = signer
	e.signFn = signFn
	
	log.Info("PoI engine authorized", "signer", signer.Hex())
}

func (e *PoIEngine) Author(header *types.Header) (common.Address, error) {
	return header.Coinbase, nil
}

func (e *PoIEngine) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	return big.NewInt(1)
}

func (e *PoIEngine) Close() error {
	// FIXED: Properly cleanup all active tasks
	e.taskMutex.Lock()
	defer e.taskMutex.Unlock()
	
	for number, task := range e.activeTasks {
		log.Debug("Cancelling active seal task", "number", number)
		task.Cancel()
	}
	e.activeTasks = make(map[uint64]*SealTask)
	e.activeTasksByUnsigned = make(map[common.Hash]*SealTask)
	
	return nil
}