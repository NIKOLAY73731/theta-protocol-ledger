package netsync

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/thetatoken/theta/common"
	"github.com/thetatoken/theta/consensus"
	"github.com/thetatoken/theta/core"
	"github.com/thetatoken/theta/ledger/state"
	"github.com/thetatoken/theta/ledger/types"
	"github.com/thetatoken/theta/rlp"
	"github.com/thetatoken/theta/store"
	"github.com/thetatoken/theta/store/database"
	"github.com/thetatoken/theta/store/kvstore"
	"github.com/thetatoken/theta/store/trie"
)

var logger *log.Entry = log.WithFields(log.Fields{"prefix": "snapshot"})

const mainnetGenesisBlockHash = "0c58bd9fa43436dc9b6887b2bcad0dbda466d90b961ffb7308773e6f815f10d9"
const genesisBlockHeight = uint64(0)

type SVStack []*state.StoreView

func (s SVStack) push(sv *state.StoreView) SVStack {
	return append(s, sv)
}

func (s SVStack) pop() (SVStack, *state.StoreView) {
	l := len(s)
	if l == 0 {
		return s, nil
	}
	return s[:l-1], s[l-1]
}

func (s SVStack) peek() *state.StoreView {
	l := len(s)
	if l == 0 {
		return nil
	}
	return s[l-1]
}

func LoadSnapshot(filePath string, db database.Database) (*core.BlockHeader, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// ------------------------- Load State ---------------------- //

	sv, hash, err := loadState(file, db)
	if err != nil {
		return nil, err
	}

	// ------------------- Validate the Snapshot ----------------- //

	metadata := core.SnapshotMetadata{}
	err = core.ReadRecord(file, &metadata)
	if err != nil {
		return nil, fmt.Errorf("Failed to load snapshot metadata, %v", err)
	}
	if err = validateSnapshot(sv, &metadata, hash, db); err != nil {
		return nil, fmt.Errorf("Snapshot validation failed: %v", err)
	}

	// --------------- Save Proofs and Tail Blocks  --------------- //

	kvstore := kvstore.NewKVStore(db)

	for i, blockTrio := range metadata.BlockTrios {
		if i < len(metadata.BlockTrios)-1 {
			blockTrioKey := []byte(core.BlockTrioStoreKeyPrefix + strconv.FormatUint(blockTrio.First.Header.Height, 64))
			kvstore.Put(blockTrioKey, blockTrio)
		}
	}

	secondBlockHeader := saveTailBlocks(&metadata, sv, kvstore)

	return secondBlockHeader, nil
}

func loadState(file *os.File, db database.Database) (*state.StoreView, common.Hash, error) {
	var hash common.Hash
	var sv *state.StoreView
	var account *types.Account
	svStack := make(SVStack, 0)
	for {
		record := core.SnapshotTrieRecord{}
		err := core.ReadRecord(file, &record)
		if err != nil {
			if err == io.EOF {
				if svStack.peek() != nil {
					return nil, common.Hash{}, fmt.Errorf("Still some storeview unhandled")
				}
				break
			}
			return nil, common.Hash{}, fmt.Errorf("Failed to read snapshot record, %v", err)
		}

		if bytes.Equal(record.K, []byte{core.SVStart}) {
			height := core.Bytestoi(record.V)
			sv := state.NewStoreView(height, common.Hash{}, db)
			svStack = svStack.push(sv)
		} else if bytes.Equal(record.K, []byte{core.SVEnd}) {
			svStack, sv = svStack.pop()
			if sv == nil {
				return nil, common.Hash{}, fmt.Errorf("Missing storeview to handle")
			}
			height := core.Bytestoi(record.V)
			if height != sv.Height() {
				return nil, common.Hash{}, fmt.Errorf("Storeview start and end heights don't match")
			}
			hash = sv.Save()

			if svStack.peek() != nil && height == svStack.peek().Height() {
				// it's a storeview for account storage, verify account
				if bytes.Compare(account.Root.Bytes(), hash.Bytes()) != 0 {
					return nil, common.Hash{}, fmt.Errorf("Account storage root doesn't match")
				}
			}
			account = nil
		} else {
			sv := svStack.peek()
			if sv == nil {
				return nil, common.Hash{}, fmt.Errorf("Missing storeview to handle")
			}
			sv.Set(record.K, record.V)

			if account == nil {
				if strings.HasPrefix(record.K.String(), "ls/a/") {
					account = &types.Account{}
					err = types.FromBytes([]byte(record.V), account)
					if err != nil {
						return nil, common.Hash{}, fmt.Errorf("Failed to parse account, %v", err)
					}
				}
			}
		}
	}

	return sv, hash, nil
}

func validateSnapshot(sv *state.StoreView, metadata *core.SnapshotMetadata, hash common.Hash, db database.Database) error {
	if bytes.Compare(metadata.BlockTrios[len(metadata.BlockTrios)-1].Second.Header.StateHash.Bytes(), hash.Bytes()) != 0 {
		return fmt.Errorf("StateHash not matching")
	}

	var provenValSet *core.ValidatorSet // the proven validator set so far
	var err error
	for idx, blockTrio := range metadata.BlockTrios {
		if blockTrio.Second.Header.Parent != blockTrio.First.Header.Hash() || blockTrio.Third.Header.Parent != blockTrio.Second.Header.Hash() {
			return fmt.Errorf("block trio has invalid Parent link")
		}

		if blockTrio.Second.Header.HCC.BlockHash != blockTrio.First.Header.Hash() || blockTrio.Third.Header.HCC.BlockHash != blockTrio.Second.Header.Hash() {
			return fmt.Errorf("block trio has invalid HCC link: %v, %v; %v, %v", blockTrio.First.Header.Hash(), blockTrio.Second.Header.HCC.BlockHash,
				blockTrio.Second.Header.Hash(), blockTrio.Third.Header.HCC.BlockHash)
		}

		firstBlock := blockTrio.First
		secondBlock := blockTrio.Second
		if idx == 0 {
			// special handling for the genesis block
			provenValSet, err = validateGenesisBlock(&secondBlock.Header, db)
			if err != nil {
				return fmt.Errorf("Invalid genesis block: %v", err)
			}
		} else {
			// blockTrio.Third.Header.HCC.Votes contains the votes for the second block in the trio
			if err := validateVotes(provenValSet, &blockTrio.Second.Header, blockTrio.Third.Header.HCC.Votes); err != nil {
				return fmt.Errorf("Failed to validate voteSet, %v", err)
			}
			provenValSet, err = getValidatorSetFromVCPProof(firstBlock.Header.StateHash, &firstBlock.Proof)
			if err != nil {
				return fmt.Errorf("Failed to retrieve validator set from VCP proof: %v", err)
			}
		}
	}

	tailBlockTrio := metadata.BlockTrios[len(metadata.BlockTrios)-1]
	validateVotes(provenValSet, &tailBlockTrio.Third.Header, tailBlockTrio.Third.VoteSet)

	retrievedValSet := getValidatorSetFromSV(sv)

	if !provenValSet.Equals(retrievedValSet) {
		return fmt.Errorf("The latest proven and retrieved validator set does not match")
	}

	return nil
}

func validateGenesisBlock(block *core.BlockHeader, db database.Database) (*core.ValidatorSet, error) {
	if block.Height != genesisBlockHeight {
		return nil, fmt.Errorf("Invalid genesis block height: %v", block.Height)
	}

	if block.Hash() != common.HexToHash(mainnetGenesisBlockHash) {
		return nil, fmt.Errorf("Genesis block hash mismatch: %v", block.Hash().Hex())
	}

	// now that the block hash matches with the expected genesis block hash,
	// the block and its state trie is considerred valid. We can retrieve the
	// genesis validator set from its state trie
	gsv := state.NewStoreView(block.Height, block.StateHash, db)

	genesisValidatorSet := getValidatorSetFromSV(gsv)

	return genesisValidatorSet, nil
}

func getValidatorSetFromVCPProof(stateHash common.Hash, recoverredVp *core.VCPProof) (*core.ValidatorSet, error) {
	serializedVCP, _, err := trie.VerifyProof(stateHash, state.ValidatorCandidatePoolKey(), recoverredVp)
	if err != nil {
		return nil, err
	}

	vcp := &core.ValidatorCandidatePool{}
	err = rlp.DecodeBytes(serializedVCP, vcp)
	if err != nil {
		return nil, err
	}
	return consensus.SelectTopStakeHoldersAsValidators(vcp), nil
}

func getValidatorSetFromSV(sv *state.StoreView) *core.ValidatorSet {
	vcp := sv.GetValidatorCandidatePool()
	return consensus.SelectTopStakeHoldersAsValidators(vcp)
}

func validateVotes(validatorSet *core.ValidatorSet, block *core.BlockHeader, voteSet *core.VoteSet) error {
	if !validatorSet.HasMajority(voteSet) {
		return fmt.Errorf("block doesn't have majority votes")
	}
	for _, vote := range voteSet.Votes() {
		res := vote.Validate()
		if !res.IsOK() {
			return fmt.Errorf("vote is not valid, %v", res)
		}
		if vote.Block != block.Hash() {
			return fmt.Errorf("vote is not for corresponding block")
		}
		_, err := validatorSet.GetValidator(vote.ID)
		if err != nil {
			return fmt.Errorf("can't find validator for vote")
		}
	}
	return nil
}

func saveTailBlocks(metadata *core.SnapshotMetadata, sv *state.StoreView, kvstore store.Store) *core.BlockHeader {
	tailBlockTrio := metadata.BlockTrios[len(metadata.BlockTrios)-1]

	firstBlock := core.Block{BlockHeader: &tailBlockTrio.First.Header}
	secondBlock := core.Block{BlockHeader: &tailBlockTrio.Second.Header}
	hl := sv.GetStakeTransactionHeightList()

	if secondBlock.Height != genesisBlockHeight {
		firstExt := core.ExtendedBlock{
			Block:              &firstBlock,
			Status:             core.BlockStatusDirectlyFinalized, // HCC links between all three blocks
			Children:           []common.Hash{secondBlock.Hash()},
			HasValidatorUpdate: hl.Contains(firstBlock.Height),
		}
		firstBlockHash := firstBlock.BlockHeader.Hash()
		kvstore.Put(firstBlockHash[:], firstExt)
	}

	secondExt := core.ExtendedBlock{
		Block:              &secondBlock,
		Status:             core.BlockStatusDirectlyFinalized,
		Children:           []common.Hash{},
		HasValidatorUpdate: hl.Contains(secondBlock.Height),
	}
	secondBlockHash := secondBlock.BlockHeader.Hash()
	kvstore.Put(secondBlockHash[:], secondExt)

	if secondExt.HasValidatorUpdate {
		// TODO: this would lead to mismatch between the proven and retrieved validator set,
		//       need to handle this case properly
		logger.Warnf("The second block in the tail trio contains validator update, may cause valSet mismatch, height: %v", secondBlock.Height)
	}

	return secondBlock.BlockHeader
}
