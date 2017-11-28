package proto

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/types"

	"github.com/NebulousLabs/writeaheadlog"
)

// A ContractSet provides safe concurrent access to a set of contracts. Its
// purpose is to serialize modifications to individual contracts, as well as
// to provide operations on the set as a whole.
type ContractSet struct {
	contracts map[types.FileContractID]*SafeContract
	wal       *writeaheadlog.WAL
	mu        sync.Mutex
}

// Acquire looks up the contract with the specified FileContractID and locks
// it before returning it. If the contract is not present in the set, Acquire
// returns false and a zero-valued RenterContract.
func (cs *ContractSet) Acquire(id types.FileContractID) (*SafeContract, bool) {
	cs.mu.Lock()
	safeContract, ok := cs.contracts[id]
	cs.mu.Unlock()
	if !ok {
		return nil, false
	}
	safeContract.mu.Lock()
	return safeContract, true
}

// Delete removes a contract from the set. The contract must have been
// previously acquired by Acquire. If the contract is not present in the set,
// Delete is a no-op.
func (cs *ContractSet) Delete(contract ContractMetadata) {
	cs.mu.Lock()
	safeContract, ok := cs.contracts[contract.ID]
	if !ok {
		cs.mu.Unlock()
		return
	}
	delete(cs.contracts, contract.ID)
	cs.mu.Unlock()

	safeContract.mu.Unlock()
}

// IDs returns the FileContractID of each contract in the set. The contracts
// are not locked.
func (cs *ContractSet) IDs() []types.FileContractID {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	ids := make([]types.FileContractID, 0, len(cs.contracts))
	for id := range cs.contracts {
		ids = append(ids, id)
	}
	return ids
}

// // Insert adds a new contract to the set. It panics if the contract is already
// // in the set.
// func (cs *ContractSet) Insert(contract modules.RenterContract) {
// 	cs.mu.Lock()
// 	defer cs.mu.Unlock()
// 	if _, ok := cs.contracts[contract.ID]; ok {
// 		build.Critical("contract already in set")
// 	}

// 	cs.contracts[contract.ID] = &safeContract{RenterContract: contract}
// }

// Len returns the number of contracts in the set.
func (cs *ContractSet) Len() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.contracts)
}

// Return returns a locked contract to the set and unlocks it. The contract
// must have been previously acquired by Acquire. If the contract is not
// present in the set, Return panics.
func (cs *ContractSet) Return(id types.FileContractID) {
	cs.mu.Lock()
	safeContract, ok := cs.contracts[id]
	if !ok {
		cs.mu.Unlock()
		build.Critical("no contract with that id")
	}
	cs.mu.Unlock()
	safeContract.mu.Unlock()
}

// View returns a copy of the contract with the specified ID. The contracts is
// not locked. Certain fields, including the MerkleRoots, are set to nil for
// safety reasons. If the contract is not present in the set, View
// returns false and a zero-valued RenterContract.
func (cs *ContractSet) View(id types.FileContractID) (ContractMetadata, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	safeContract, ok := cs.contracts[id]
	if !ok {
		return ContractMetadata{}, false
	}
	return safeContract.Metadata(), true
}

// ViewAll returns the metadata of each contract in the set. The contracts are
// not locked.
func (cs *ContractSet) ViewAll() []ContractMetadata {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	contracts := make([]ContractMetadata, 0, len(cs.contracts))
	for _, safeContract := range cs.contracts {
		contracts = append(contracts, safeContract.Metadata())
	}
	return contracts
}

func (cs *ContractSet) Close() error {
	for _, c := range cs.contracts {
		c.f.Close()
	}
	return cs.wal.Close()
}

// NewContractSet returns a ContractSet storing its contracts in the specified
// dir.
func NewContractSet(dir string) (*ContractSet, error) {
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	} else if stat, err := d.Stat(); err != nil {
		return nil, err
	} else if !stat.IsDir() {
		return nil, errors.New("not a directory")
	}
	defer d.Close()

	// Load the WAL. Any recovered updates will be applied after loading
	// contracts.
	updates, wal, err := writeaheadlog.New(filepath.Join(dir, "contractset.log"))
	if err != nil {
		return nil, err
	}

	// Load the contract files.
	dirNames, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	set := make(map[types.FileContractID]*SafeContract)
	for _, filename := range dirNames {
		if filepath.Ext(filename) != "contract" {
			continue
		}
		c, err := loadSafeContract(filename)
		if err != nil {
			return nil, err
		}
		c.wal = wal
		set[c.Metadata().ID] = c
	}

	// Apply any recovered updates.
	if err := applyRecoveredUpdates(set, updates); err != nil {
		return nil, err
	}

	return &ContractSet{
		contracts: set,
		wal:       wal,
	}, nil
}

func applyRecoveredUpdates(set map[types.FileContractID]*SafeContract, updates []writeaheadlog.Update) error {
	for _, update := range updates {
		switch update.Name {
		case updateNameSetHeader:
			var u updateSetHeader
			if err := encoding.Unmarshal(update.Instructions, &u); err != nil {
				return err
			}
			if c, ok := set[u.ID]; !ok {
				return errors.New("no such contract")
			} else if err := c.applySetHeader(u.Header); err != nil {
				return err
			} else if err := c.f.Sync(); err != nil {
				return err
			}
		case updateNameSetRoot:
			var u updateSetRoot
			if err := encoding.Unmarshal(update.Instructions, &u); err != nil {
				return err
			}
			if c, ok := set[u.ID]; !ok {
				return errors.New("no such contract")
			} else if err := c.applySetRoot(u.Root, u.Index); err != nil {
				return err
			} else if err := c.f.Sync(); err != nil {
				return err
			}
		}
	}
	return nil
}
