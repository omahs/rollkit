package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/celestiaorg/go-fraud/fraudserv"
	abci "github.com/cometbft/cometbft/abci/types"
	cryptoenc "github.com/cometbft/cometbft/crypto/encoding"
	cmbytes "github.com/cometbft/cometbft/libs/bytes"
	cmstate "github.com/cometbft/cometbft/proto/tendermint/state"
	cmproto "github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/cometbft/cometbft/proxy"
	cmtypes "github.com/cometbft/cometbft/types"

	abciconv "github.com/rollkit/rollkit/conv/abci"
	"github.com/rollkit/rollkit/log"
	"github.com/rollkit/rollkit/mempool"
	"github.com/rollkit/rollkit/types"
)

var ErrFraudProofGenerated = errors.New("failed to ApplyBlock: halting node due to fraud")
var ErrEmptyValSetGenerated = errors.New("applying the validator changes would result in empty set")
var ErrAddingValidatorToBased = errors.New("cannot add validators to empty validator set")

// BlockExecutor creates and applies blocks and maintains state.
type BlockExecutor struct {
	proposerAddress    []byte
	namespaceID        types.NamespaceID
	chainID            string
	proxyApp           proxy.AppConns
	mempool            mempool.Mempool
	fraudProofsEnabled bool

	eventBus *cmtypes.EventBus

	logger log.Logger

	FraudService *fraudserv.ProofService
}

// NewBlockExecutor creates new instance of BlockExecutor.
// Proposer address and namespace ID will be used in all newly created blocks.
func NewBlockExecutor(proposerAddress []byte, namespaceID [8]byte, chainID string, mempool mempool.Mempool, proxyApp proxy.AppConns, fraudProofsEnabled bool, eventBus *cmtypes.EventBus, logger log.Logger) *BlockExecutor {
	return &BlockExecutor{
		proposerAddress:    proposerAddress,
		namespaceID:        namespaceID,
		chainID:            chainID,
		proxyApp:           proxyApp,
		mempool:            mempool,
		fraudProofsEnabled: fraudProofsEnabled,
		eventBus:           eventBus,
		logger:             logger,
	}
}

// InitChain calls InitChainSync using consensus connection to app.
func (e *BlockExecutor) InitChain(ctx context.Context, genesis *cmtypes.GenesisDoc) (*abci.ResponseInitChain, error) {
	params := genesis.ConsensusParams

	validators := make([]*cmtypes.Validator, len(genesis.Validators))
	for i, v := range genesis.Validators {
		validators[i] = cmtypes.NewValidator(v.PubKey, v.Power)
	}

	return e.proxyApp.Consensus().InitChain(ctx, &abci.RequestInitChain{
		Time:    genesis.GenesisTime,
		ChainId: genesis.ChainID,
		ConsensusParams: &cmproto.ConsensusParams{
			Block: &cmproto.BlockParams{
				MaxBytes: params.Block.MaxBytes,
				MaxGas:   params.Block.MaxGas,
			},
			Evidence: &cmproto.EvidenceParams{
				MaxAgeNumBlocks: params.Evidence.MaxAgeNumBlocks,
				MaxAgeDuration:  params.Evidence.MaxAgeDuration,
				MaxBytes:        params.Evidence.MaxBytes,
			},
			Validator: &cmproto.ValidatorParams{
				PubKeyTypes: params.Validator.PubKeyTypes,
			},
			Version: &cmproto.VersionParams{
				App: params.Version.App,
			},
		},
		Validators:    cmtypes.TM2PB.ValidatorUpdates(cmtypes.NewValidatorSet(validators)),
		AppStateBytes: genesis.AppState,
		InitialHeight: genesis.InitialHeight,
	})
}

// CreateBlock reaps transactions from mempool and builds a block.
func (e *BlockExecutor) CreateBlock(ctx context.Context, height uint64, lastCommit *types.Commit, lastHeaderHash types.Hash, state types.State) *types.Block {
	maxBytes := state.ConsensusParams.Block.MaxBytes
	maxGas := state.ConsensusParams.Block.MaxGas

	mempoolTxs := e.mempool.ReapMaxBytesMaxGas(maxBytes, maxGas)

	block := &types.Block{
		SignedHeader: types.SignedHeader{
			Header: types.Header{
				Version: types.Version{
					Block: state.Version.Consensus.Block,
					App:   state.Version.Consensus.App,
				},
				BaseHeader: types.BaseHeader{
					ChainID: e.chainID,
					Height:  height,
					Time:    uint64(time.Now().Unix()), // TODO(tzdybal): how to get TAI64?
				},
				//LastHeaderHash: lastHeaderHash,
				//LastCommitHash:  lastCommitHash,
				DataHash:        make(types.Hash, 32),
				ConsensusHash:   make(types.Hash, 32),
				AppHash:         state.AppHash,
				LastResultsHash: state.LastResultsHash,
				ProposerAddress: e.proposerAddress,
			},
			Commit: *lastCommit,
		},
		Data: types.Data{
			Txs:                    toRollkitTxs(mempoolTxs),
			IntermediateStateRoots: types.IntermediateStateRoots{RawRootsList: nil},
			// Note: Temporarily remove Evidence #896
			// Evidence:               types.EvidenceData{Evidence: nil},
		},
	}
	block.SignedHeader.Header.LastCommitHash = e.getLastCommitHash(lastCommit, &block.SignedHeader.Header)
	block.SignedHeader.Header.LastHeaderHash = lastHeaderHash
	block.SignedHeader.Header.AggregatorsHash = state.Validators.Hash()

	return block
}

// ApplyBlock validates and executes the block.
func (e *BlockExecutor) ApplyBlock(ctx context.Context, state types.State, block *types.Block) (types.State, *cmstate.LegacyABCIResponses, error) {
	err := e.validate(state, block)
	if err != nil {
		return types.State{}, nil, err
	}

	// This makes calls to the AppClient
	resp, err := e.execute(ctx, state, block)
	if err != nil {
		return types.State{}, nil, err
	}

	abciValUpdates := resp.EndBlock.ValidatorUpdates

	protoState, err := state.ToProto()
	if err != nil {
		return types.State{}, nil, err
	}

	err = validateValidatorUpdates(abciValUpdates, protoState.ConsensusParams.Validator)
	if err != nil {
		return state, nil, fmt.Errorf("error in validator updates: %v", err)
	}

	validatorUpdates, err := cmtypes.PB2TM.ValidatorUpdates(abciValUpdates)
	if err != nil {
		return state, nil, err
	}
	if len(validatorUpdates) > 0 {
		e.logger.Debug("updates to validators", "updates", cmtypes.ValidatorListString(validatorUpdates))
	}
	if state.ConsensusParams.Block.MaxBytes == 0 {
		e.logger.Error("maxBytes=0", "state.ConsensusParams.Block", state.ConsensusParams.Block, "block", block)
	}

	state, err = e.updateState(state, block, resp, validatorUpdates)
	if err != nil {
		return types.State{}, nil, err
	}

	return state, resp, nil
}

// Commit commits the block
func (e *BlockExecutor) Commit(ctx context.Context, state types.State, block *types.Block, resp *cmstate.LegacyABCIResponses) ([]byte, uint64, error) {
	appHash, retainHeight, err := e.commit(ctx, state, block, resp.DeliverTxs)
	if err != nil {
		return []byte{}, 0, err
	}

	state.AppHash = appHash

	e.publishEvents(resp, block, state)

	return appHash, retainHeight, nil
}

func (e *BlockExecutor) VerifyFraudProof(ctx context.Context, fraudProof *abci.FraudProof, expectedValidAppHash []byte) (bool, error) {
	resp, err := e.proxyApp.Consensus().VerifyFraudProof(
		ctx,
		&abci.RequestVerifyFraudProof{
			FraudProof:           fraudProof,
			ExpectedValidAppHash: expectedValidAppHash,
		},
	)
	if err != nil {
		return false, err
	}
	return resp.Success, nil
}

func (e *BlockExecutor) SetFraudProofService(fraudProofServ *fraudserv.ProofService) {
	e.FraudService = fraudProofServ
}

func (e *BlockExecutor) updateState(state types.State, block *types.Block, abciResponses *cmstate.LegacyABCIResponses, validatorUpdates []*cmtypes.Validator) (types.State, error) {
	nValSet := state.NextValidators.Copy()
	lastHeightValSetChanged := state.LastHeightValidatorsChanged
	// Rollkit can work without validators
	if len(nValSet.Validators) > 0 {
		if len(validatorUpdates) > 0 {
			err := nValSet.UpdateWithChangeSet(validatorUpdates)
			if err != nil {
				if err.Error() != ErrEmptyValSetGenerated.Error() {
					return state, err
				}
				nValSet = &cmtypes.ValidatorSet{
					Validators: make([]*cmtypes.Validator, 0),
					Proposer:   nil,
				}
			}
			// Change results from this height but only applies to the next next height.
			lastHeightValSetChanged = block.SignedHeader.Header.Height() + 1 + 1
		}

		if len(nValSet.Validators) > 0 {
			nValSet.IncrementProposerPriority(1)
		}
		// TODO(tzdybal):  right now, it's for backward compatibility, may need to change this
	} else if len(validatorUpdates) > 0 {
		return state, ErrAddingValidatorToBased
	}

	s := types.State{
		Version:         state.Version,
		ChainID:         state.ChainID,
		InitialHeight:   state.InitialHeight,
		LastBlockHeight: block.SignedHeader.Header.Height(),
		LastBlockTime:   block.SignedHeader.Header.Time(),
		LastBlockID: cmtypes.BlockID{
			Hash: cmbytes.HexBytes(block.SignedHeader.Header.Hash()),
			// for now, we don't care about part set headers
		},
		NextValidators:                   nValSet,
		Validators:                       nValSet,
		LastValidators:                   state.Validators.Copy(),
		LastHeightValidatorsChanged:      lastHeightValSetChanged,
		ConsensusParams:                  state.ConsensusParams,
		LastHeightConsensusParamsChanged: state.LastHeightConsensusParamsChanged,
		AppHash:                          make(types.Hash, 32),
	}

	copy(s.LastResultsHash[:], cmtypes.NewResults(abciResponses.DeliverTxs).Hash())

	return s, nil
}

func (e *BlockExecutor) commit(ctx context.Context, state types.State, block *types.Block, deliverTxs []*abci.ExecTxResult) ([]byte, uint64, error) {
	e.mempool.Lock()
	defer e.mempool.Unlock()

	err := e.mempool.FlushAppConn()
	if err != nil {
		return nil, 0, err
	}
	commitResp, err := e.proxyApp.Consensus().Commit(ctx)
	if err != nil {
		return nil, 0, err
	}

	appHash, err := e.proxyApp.Consensus().GetAppHash(ctx, &abci.RequestGetAppHash{})
	if err != nil {
		return nil, 0, err
	}

	maxBytes := state.ConsensusParams.Block.MaxBytes
	maxGas := state.ConsensusParams.Block.MaxGas
	err = e.mempool.Update(int64(block.SignedHeader.Header.Height()), fromRollkitTxs(block.Data.Txs), deliverTxs, mempool.PreCheckMaxBytes(maxBytes), mempool.PostCheckMaxGas(maxGas))
	if err != nil {
		return nil, 0, err
	}
	return appHash.AppHash, uint64(commitResp.RetainHeight), err
}

func (e *BlockExecutor) validate(state types.State, block *types.Block) error {
	err := block.ValidateBasic()
	if err != nil {
		return err
	}
	if block.SignedHeader.Header.Version.App != state.Version.Consensus.App ||
		block.SignedHeader.Header.Version.Block != state.Version.Consensus.Block {
		return errors.New("block version mismatch")
	}
	if state.LastBlockHeight <= 0 && block.SignedHeader.Header.Height() != state.InitialHeight {
		return errors.New("initial block height mismatch")
	}
	if state.LastBlockHeight > 0 && block.SignedHeader.Header.Height() != state.LastBlockHeight+1 {
		return errors.New("block height mismatch")
	}
	if !bytes.Equal(block.SignedHeader.Header.AppHash[:], state.AppHash[:]) {
		return errors.New("AppHash mismatch")
	}

	if !bytes.Equal(block.SignedHeader.Header.LastResultsHash[:], state.LastResultsHash[:]) {
		return errors.New("LastResultsHash mismatch")
	}

	if !bytes.Equal(block.SignedHeader.Header.AggregatorsHash[:], state.Validators.Hash()) {
		return errors.New("AggregatorsHash mismatch")
	}

	return nil
}

func (e *BlockExecutor) execute(ctx context.Context, state types.State, block *types.Block) (*cmstate.LegacyABCIResponses, error) {
	abciResponses := new(cmstate.LegacyABCIResponses)

	abciResponses.DeliverTxs = make([]*abci.ExecTxResult, len(block.Data.Txs))

	txIdx := 0
	validTxs := 0
	invalidTxs := 0

	currentIsrs := block.Data.IntermediateStateRoots.RawRootsList
	currentIsrIndex := 0

	if e.fraudProofsEnabled && currentIsrs != nil {
		expectedLength := len(block.Data.Txs) + 3 // before BeginBlock, after BeginBlock, after every Tx, after EndBlock
		if len(currentIsrs) != expectedLength {
			return nil, fmt.Errorf("invalid length of ISR list: %d, expected length: %d", len(currentIsrs), expectedLength)
		}
	}

	ISRs := make([][]byte, 0)

	if e.fraudProofsEnabled {
		isr, err := e.getAppHash()
		if err != nil {
			return nil, err
		}
		ISRs = append(ISRs, isr)
		currentIsrIndex++
	}

	genAndGossipFraudProofIfNeeded := func(beginBlockRequest *abci.RequestBeginBlock, deliverTxRequests []*abci.RequestDeliverTx, endBlockRequest *abci.RequestEndBlock) (err error) {
		if !e.fraudProofsEnabled {
			return nil
		}
		isr, err := e.getAppHash()
		if err != nil {
			return err
		}
		ISRs = append(ISRs, isr)
		isFraud := e.isFraudProofTrigger(isr, currentIsrs, currentIsrIndex)
		if isFraud {
			e.logger.Info("found fraud occurrence, generating a fraud proof...")
			fraudProof, err := e.generateFraudProof(beginBlockRequest, deliverTxRequests, endBlockRequest)
			if err != nil {
				return err
			}
			// Gossip Fraud Proof
			if err := e.FraudService.Broadcast(ctx, &types.StateFraudProof{FraudProof: *fraudProof}); err != nil {
				return fmt.Errorf("failed to broadcast fraud proof: %w", err)
			}
			return ErrFraudProofGenerated
		}
		currentIsrIndex++
		return nil
	}

	hash := block.Hash()
	abciHeader, err := abciconv.ToABCIHeaderPB(&block.SignedHeader.Header)
	if err != nil {
		return nil, err
	}
	abciHeader.ChainID = e.chainID
	abciHeader.ValidatorsHash = state.Validators.Hash()

	abciBlock, err := abciconv.ToABCIBlock(block)
	if err != nil {
		return nil, err
	}

	// perform the FinalizeBlock request and get a LegacyABCI response from it
	// see if we can use those responses to deal with all the fraud proof stuff
	// and add a method to save all the relevant changes into the state.
	finalizeBlockResponse, err := e.proxyApp.Consensus().FinalizeBlock(context.TODO(), &abci.RequestFinalizeBlock{
		Hash:               block.Hash(),
		NextValidatorsHash: abciHeader.NextValidatorsHash,
		ProposerAddress:    abciHeader.ProposerAddress,
		Height:             abciHeader.Height,
		Time:               abciHeader.Time,
		DecidedLastCommit: abci.CommitInfo{
			Round: 0,
			Votes: nil,
		},
		Misbehavior: abciBlock.Evidence.Evidence.ToABCI(),
		Txs:         abciBlock.Txs.ToSliceOfBytes(),
	})

	if err != nil {
		e.logger.Error("error in proxyAppConn.FinalizeBlock", "err", err)
		return nil, err
	}

	e.logger.Info(
		"finalized block",
		"height", abciBlock.Height,
		"num_txs_res", len(finalizeBlockResponse.TxResults),
		"num_val_updates", len(finalizeBlockResponse.ValidatorUpdates),
		"block_app_hash", fmt.Sprintf("%X", finalizeBlockResponse.AppHash),
	)

	// Assert that the application correctly returned tx results for each of the transactions provided in the block
	if len(abciBlock.Data.Txs) != len(finalizeBlockResponse.TxResults) {
		return nil, fmt.Errorf("expected tx results length to match size of transactions in block. Expected %d, got %d", len(block.Data.Txs), len(finalizeBlockResponse.TxResults))
	}

	e.logger.Info("executed block", "height", abciHeader.Height, "app_hash", finalizeBlockResponse.AppHash)

	// we add all the block events to EndBlock, since we can no longer distinguish when events happened
	// and events in EndBlock better simulate the FinalizeBlock Event
	abciResponses.BeginBlock = &cmstate.ResponseBeginBlock{
		Events: make([]abci.Event, 0),
	}

	for _, execResult := range finalizeBlockResponse.TxResults {
		if execResult.Code == abci.CodeTypeOK {
			validTxs++
		} else {
			e.logger.Debug("Invalid tx", "code", execResult.Code, "log", execResult.Log)
			invalidTxs++
		}
		abciResponses.DeliverTxs[txIdx] = &abci.ExecTxResult{
			Code:      execResult.Code,
			Data:      execResult.Data,
			Log:       execResult.Log,
			Info:      execResult.Info,
			GasWanted: execResult.GasWanted,
			GasUsed:   execResult.GasUsed,
			Events:    execResult.Events,
			Codespace: execResult.Codespace,
		}
		txIdx++
	}

	beginBlockRequest := abci.RequestBeginBlock{
		Hash:   hash[:],
		Header: abciHeader,
		LastCommitInfo: abci.CommitInfo{
			Round: 0,
			Votes: nil,
		},
		ByzantineValidators: nil,
	}

	err = genAndGossipFraudProofIfNeeded(&beginBlockRequest, nil, nil)
	if err != nil {
		return nil, err
	}

	deliverTxRequests := make([]*abci.RequestDeliverTx, 0, len(block.Data.Txs))
	for _, tx := range block.Data.Txs {
		deliverTxRequest := abci.RequestDeliverTx{Tx: tx}
		deliverTxRequests = append(deliverTxRequests, &deliverTxRequest)

		err = genAndGossipFraudProofIfNeeded(&beginBlockRequest, deliverTxRequests, nil)
		if err != nil {
			return nil, err
		}
	}
	endBlockRequest := abci.RequestEndBlock{Height: block.SignedHeader.Header.Height()}

	// we write all the events in EndBlock, since we can't tell if the events occur during
	// a hypotethical BeginBlock or EndBlock.
	// All of this should change to use FinalizeBlock
	abciResponses.EndBlock = &cmstate.ResponseEndBlock{
		ValidatorUpdates:      finalizeBlockResponse.GetValidatorUpdates(),
		ConsensusParamUpdates: finalizeBlockResponse.GetConsensusParamUpdates(),
		Events:                finalizeBlockResponse.GetEvents(),
	}

	err = genAndGossipFraudProofIfNeeded(&beginBlockRequest, deliverTxRequests, &endBlockRequest)
	if err != nil {
		return nil, err
	}

	if e.fraudProofsEnabled && block.Data.IntermediateStateRoots.RawRootsList == nil {
		// Block producer: Initial ISRs generated here
		block.Data.IntermediateStateRoots.RawRootsList = ISRs
	}

	return abciResponses, nil
}

func (e *BlockExecutor) isFraudProofTrigger(generatedIsr []byte, currentIsrs [][]byte, index int) bool {
	if currentIsrs == nil {
		return false
	}
	stateIsr := currentIsrs[index]
	if !bytes.Equal(stateIsr, generatedIsr) {
		e.logger.Debug("ISR Mismatch", "given_isr", stateIsr, "generated_isr", generatedIsr)
		return true
	}
	return false
}

func (e *BlockExecutor) generateFraudProof(beginBlockRequest *abci.RequestBeginBlock, deliverTxRequests []*abci.RequestDeliverTx, endBlockRequest *abci.RequestEndBlock) (*abci.FraudProof, error) {
	generateFraudProofRequest := abci.RequestGenerateFraudProof{}
	if beginBlockRequest == nil {
		return nil, fmt.Errorf("begin block request cannot be a nil parameter")
	}
	generateFraudProofRequest.BeginBlockRequest = *beginBlockRequest
	if deliverTxRequests != nil {
		generateFraudProofRequest.DeliverTxRequests = deliverTxRequests
		if endBlockRequest != nil {
			generateFraudProofRequest.EndBlockRequest = endBlockRequest
		}
	}

	ctx := context.Background()
	resp, err := e.proxyApp.Consensus().GenerateFraudProof(ctx, &generateFraudProofRequest)
	if err != nil {
		return nil, err
	}
	if resp.FraudProof == nil {
		return nil, fmt.Errorf("fraud proof generation failed")
	}
	return resp.FraudProof, nil
}

func (e *BlockExecutor) getLastCommitHash(lastCommit *types.Commit, header *types.Header) []byte {
	lastABCICommit := abciconv.ToABCICommit(lastCommit, header.BaseHeader.Height, header.Hash())
	if len(lastCommit.Signatures) == 1 {
		lastABCICommit.Signatures[0].ValidatorAddress = e.proposerAddress
		lastABCICommit.Signatures[0].Timestamp = header.Time()
	}
	return lastABCICommit.Hash()
}

func (e *BlockExecutor) publishEvents(resp *cmstate.LegacyABCIResponses, block *types.Block, state types.State) {
	if e.eventBus == nil {
		return
	}

	abciBlock, err := abciconv.ToABCIBlock(block)

	abciBlock.Header.ValidatorsHash = state.Validators.Hash()
	if err != nil {
		return
	}

	finalizeBlockResp := abci.ResponseFinalizeBlock{
		TxResults:             resp.DeliverTxs,
		ValidatorUpdates:      resp.EndBlock.ValidatorUpdates,
		ConsensusParamUpdates: resp.EndBlock.ConsensusParamUpdates,
		Events:                append(resp.BeginBlock.Events, resp.EndBlock.Events...),
		AppHash:               state.AppHash,
	}
	if err := e.eventBus.PublishEventNewBlock(cmtypes.EventDataNewBlock{
		Block: abciBlock,
		BlockID: cmtypes.BlockID{
			Hash: cmbytes.HexBytes(block.SignedHeader.Header.Hash()),
			// for now, we don't care about part set headers
		},
		ResultFinalizeBlock: finalizeBlockResp,
	}); err != nil {
		e.logger.Error("failed publishing new block", "err", err)
	}

	if err := e.eventBus.PublishEventNewBlockHeader(cmtypes.EventDataNewBlockHeader{
		Header: abciBlock.Header,
	}); err != nil {
		e.logger.Error("failed publishing new block header", "err", err)
	}

	if err := e.eventBus.PublishEventNewBlockEvents(cmtypes.EventDataNewBlockEvents{
		Height: abciBlock.Height,
		Events: finalizeBlockResp.Events,
		NumTxs: int64(len(abciBlock.Txs)),
	}); err != nil {
		e.logger.Error("failed publishing new block events", "err", err)
	}

	if len(abciBlock.Evidence.Evidence) != 0 {
		for _, ev := range abciBlock.Evidence.Evidence {
			if err := e.eventBus.PublishEventNewEvidence(cmtypes.EventDataNewEvidence{
				Evidence: ev,
				Height:   block.SignedHeader.Header.Height(),
			}); err != nil {
				e.logger.Error("failed publishing new evidence", "err", err)
			}
		}
	}

	for i, tx := range abciBlock.Data.Txs {
		err := e.eventBus.PublishEventTx(cmtypes.EventDataTx{
			TxResult: abci.TxResult{
				Height: abciBlock.Height,
				Index:  uint32(i),
				Tx:     tx,
				Result: *(finalizeBlockResp.TxResults[i]),
			},
		})
		if err != nil {
			e.logger.Error("failed publishing event TX", "err", err)
		}
	}
}

func (e *BlockExecutor) getAppHash() ([]byte, error) {
	ctx := context.Background()
	isrResp, err := e.proxyApp.Consensus().GetAppHash(ctx, &abci.RequestGetAppHash{})
	if err != nil {
		return nil, err
	}
	return isrResp.AppHash, nil
}

func toRollkitTxs(txs cmtypes.Txs) types.Txs {
	rollkitTxs := make(types.Txs, len(txs))
	for i := range txs {
		rollkitTxs[i] = []byte(txs[i])
	}
	return rollkitTxs
}

func fromRollkitTxs(rollkitTxs types.Txs) cmtypes.Txs {
	txs := make(cmtypes.Txs, len(rollkitTxs))
	for i := range rollkitTxs {
		txs[i] = []byte(rollkitTxs[i])
	}
	return txs
}

func validateValidatorUpdates(abciUpdates []abci.ValidatorUpdate, params *cmproto.ValidatorParams) error {
	for _, valUpdate := range abciUpdates {
		if valUpdate.GetPower() < 0 {
			return fmt.Errorf("voting power can't be negative %v", valUpdate)
		} else if valUpdate.GetPower() == 0 {
			// continue, since this is deleting the validator, and thus there is no
			// pubkey to check
			continue
		}

		// Check if validator's pubkey matches an ABCI type in the consensus params
		pk, err := cryptoenc.PubKeyFromProto(valUpdate.PubKey)
		if err != nil {
			return err
		}

		if !cmtypes.IsValidPubkeyType(cmtypes.ValidatorParams{PubKeyTypes: params.PubKeyTypes}, pk.Type()) {
			return fmt.Errorf("validator %v is using pubkey %s, which is unsupported for consensus",
				valUpdate, pk.Type())
		}
	}
	return nil
}
