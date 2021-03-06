package common

import (
	"fmt"

	"github.com/MixinNetwork/mixin/crypto"
)

type DepositData struct {
	Chain           crypto.Hash `json:"chain"`
	AssetKey        string      `json:"asset"`
	TransactionHash string      `json:"transaction"`
	OutputIndex     uint64      `json:"index"`
	Amount          Integer     `json:"amount"`
}

func (tx *SignedTransaction) verifyDepositFormat() error {
	deposit := tx.Inputs[0].Deposit
	err := deposit.validateEthereumAssetInput()
	if err != nil {
		return err
	}
	if deposit.AssetId() != tx.Asset {
		return fmt.Errorf("invalid asset %s %s", tx.Asset, deposit.AssetId())
	}
	return nil
}

func (tx *SignedTransaction) validateDepositInput(store DataStore, msg []byte) error {
	if len(tx.Inputs) != 1 {
		return fmt.Errorf("invalid inputs count %d for deposit", len(tx.Inputs))
	}
	if len(tx.Signatures) != 1 || len(tx.Signatures[0]) != 1 {
		return fmt.Errorf("invalid signatures count %d for deposit", len(tx.Signatures))
	}
	err := tx.verifyDepositFormat()
	if err != nil {
		return err
	}

	sig, valid := tx.Signatures[0][0], false
	domains := store.ReadDomains()
	for _, d := range domains {
		if d.Account.PublicSpendKey.Verify(msg, sig) {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("invalid domain signature for deposit")
	}
	return nil
}

func (tx *Transaction) AddDepositInput(data *DepositData) {
	tx.Inputs = append(tx.Inputs, &Input{
		Deposit: data,
	})
}
