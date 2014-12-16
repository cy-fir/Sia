package wallet

import (
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/NebulousLabs/Andromeda/consensus"
	"github.com/NebulousLabs/Andromeda/signatures"
)

// openTransaction is a type that the wallet uses to track a transaction as it
// adds inputs and other features.
type openTransaction struct {
	transaction *consensus.Transaction
	inputs      []uint64
}

// openOutput contains an output and the conditions needed to spend the output,
// including secret keys.
type spendableAddress struct {
	outputs         []*consensus.Output
	spendConditions *consensus.SpendConditions
	secretKey       signatures.SecretKey
}

// Wallet holds your coins, manages privacy, outputs, ect. The balance reported
// by the wallet does not include coins that you have spent in transactions yet
// haven't been revealed in a block.
//
// TODO: Right now, the Wallet stores all of the outputs itself, because it
// doesn't have access to the state. There should probably be some abstracted
// object which can do that for the Wallet, which is shared between all of the
// things that need to do the lookups. (and type consensus.State would
// implement the interface fulfilling that abstraction)
type Wallet struct {
	balance            consensus.Currency
	ownedOutputs       map[consensus.CoinAddress]struct{}
	spentOutputs       map[consensus.CoinAddress]struct{}
	spendableAddresses map[consensus.CoinAddress]*spendableAddress

	transactionCounter int
	transactions       map[string]*openTransaction

	sync.RWMutex
}

// New creates an initializes a Wallet.
func New() (*Wallet, error) {
	return &Wallet{
		ownedOutputs:       make(map[consensus.CoinAddress]struct{}),
		spentOutputs:       make(map[consensus.CoinAddress]struct{}),
		spendableAddresses: make(map[consensus.CoinAddress]*spendableAddress),
		transactions:       make(map[string]*openTransaction),
	}, nil
}

// Update implements the core.Wallet interface.
func (w *Wallet) Update(rewound []consensus.Block, applied []consensus.Block) error {
	w.Lock()
	defer w.Unlock()

	// Remove all of the owned outputs created in the rewound blocks. Do not
	// change the spent outputs map.
	for _, b := range rewound {
		for i := len(b.Transactions) - 1; i >= 0; i-- {
			// Remove all outputs that got created by this block.
			for j, _ := range b.Transactions[i].Outputs {
				id := b.Transactions[i].OutputID(j)
				delete(w.ownedOutputs, id)
			}

			// Re-add all inputs that got consumed by this block.
			for _, input := range b.Transactions[i].Inputs {
				if ca == input.SpendConditions.CoinAddress() {
					w.balance += w.outputs[input.OutputID].output.Value
					w.ownedOutputs[input.OutputID] = struct{}{}
				}
			}
		}
	}

	// Add all of the owned outputs created in applied blocks, and remove all
	// of the owned outputs that got consumed.
	for _, b := range applied {
		for _, t := range b.Transactions {
			// Remove all the outputs that got consumed by this block.
			for _, input := range t.Inputs {
				delete(w.ownedOutputs, input.OutputID)
			}

			// Add all of the outputs that got created by this block.
			for i, output := range t.Outputs {
				if ca == output.SpendHash {
					id := t.OutputID(i)
					w.ownedOutputs[id] = struct{}{}
					w.outputs[id].output = &output
					w.balance += output.Value
				}
			}
		}
	}

	return nil
}

// Reset implements the core.Wallet interface.
func (w *Wallet) Reset() error {
	w.Lock()
	defer w.Unlock()

	for id := range w.spentOutputs {
		// Add the spent output back into the balance if it's currently an
		// owned output.
		if _, exists := w.ownedOutputs[id]; exists {
			w.balance += w.outputs[id].output.Value
		}
		delete(w.spentOutputs, id)
	}
	return nil
}

// Balance implements the core.Wallet interface.
func (w *Wallet) Balance() (consensus.Currency, error) {
	w.RLock()
	defer w.RUnlock()
	return w.balance, nil
}

// CoinAddress implements the core.Wallet interface.
func (w *Wallet) CoinAddress() (coinAddress consensus.CoinAddress, err error) {
	sk, pk, err := signatures.GenerateKeyPair()
	if err != nil {
		return
	}

	newSpendableAddress := &spendableAddress{
		spendConditions: consensus.SpendConditions{
			NumSignatures: 1,
			PublicKeys:    []signatures.PublicKey{pk},
		},
		secretKey: sk,
	}

	coinAddress = newAddress.spendConditions.CoinAddress()
	w.spendableAddresses[coinAddress] = newSpendableAddress
	return
}

// RegisterTransaction implements the core.Wallet interface.
func (w *Wallet) RegisterTransaction(t *consensus.Transaction) (id string, err error) {
	w.Lock()
	defer w.Unlock()

	id = strconv.Itoa(w.transactionCounter)
	w.transactionCounter++
	w.transactions[id].transaction = t
	return
}

// FundTransaction implements the core.Wallet interface.
func (w *Wallet) FundTransaction(id string, amount consensus.Currency) error {
	if amount == consensus.Currency(0) {
		return errors.New("cannot fund 0 coins") // should this be an error or nil?
	}
	ot, exists := w.transactions[id]
	if !exists {
		return errors.New("no transaction of given id found")
	}
	t := ot.transaction

	total := consensus.Currency(0)
	var newInputs []consensus.Input
	for id, _ := range w.ownedOutputs {
		// Check if we've already spent the output.
		_, exists := w.spentOutputs[id]
		if exists {
			continue
		}

		// Fetch the output
		output := w.outputs[id].output

		// Create an input for the transaction
		newInput := consensus.Input{
			OutputID:        id,
			SpendConditions: w.spendConditions,
		}
		newInputs = append(newInputs, newInput)

		// See if the value of the inputs has surpassed `amount`.
		total += output.Value
		if total >= amount {
			break
		}
	}

	// Check that enough inputs were added.
	if total < amount {
		return fmt.Errorf("insufficient funds, requested %v but only have %v", amount, total)
	}

	// Add the inputs to the transaction.
	t.Inputs = append(t.Inputs, newInputs...)
	for _, input := range newInputs {
		ot.inputs = append(ot.inputs, uint64(len(t.Inputs)))
		w.spentOutputs[input.OutputID] = struct{}{}
	}

	// Add a refund output if needed.
	if total-amount > 0 {
		t.Outputs = append(
			t.Outputs,
			consensus.Output{
				Value:     total - amount,
				SpendHash: w.spendConditions.CoinAddress(),
			},
		)
	}

	return nil
}

// AddMinerFee implements the core.Wallet interface.
func (w *Wallet) AddMinerFee(id string, fee consensus.Currency) error {
	to, exists := w.transactions[id]
	if !exists {
		return errors.New("no transaction found for given id")
	}

	to.transaction.MinerFees = append(to.transaction.MinerFees, fee)
	return nil
}

// AddOutput implements the core.Wallet interface.
func (w *Wallet) AddOutput(id string, amount consensus.Currency, dest consensus.CoinAddress) error {
	to, exists := w.transactions[id]
	if !exists {
		return errors.New("no transaction found for given id")
	}

	to.transaction.Outputs = append(to.transaction.Outputs, consensus.Output{Value: amount, SpendHash: dest})
	return nil
}
