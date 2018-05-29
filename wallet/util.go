package wallet

import (
	"crypto/ecdsa"
	"fmt"

	"github.com/btcsuite/btcd/btcec"

	"gx/ipfs/QmVmDhyTTUcQXFD1rRQ64fGLMSAoaQvNH3hwuaCFAPq2hy/errors"

	"github.com/filecoin-project/go-filecoin/crypto"
)

// Sign cryptographically signs `data` using the private key of address `addr`.
// TODO Zero out the sensitive data when complete
func sign(priv *btcec.PrivateKey, hash []byte) ([]byte, error) {

	// sign the content
	sig, err := crypto.Sign(hash[:], (*ecdsa.PrivateKey)(priv))
	if err != nil {
		return nil, errors.Wrap(err, "Failed to sign data")
	}

	fmt.Printf("\nSIGN - \nsk:\t%x\npk:\t%x\nsig:\t%x\nhash:\t%x\n\n", priv.Serialize(), priv.PubKey().SerializeUncompressed(), sig, hash[:])
	return sig, nil
}

// Verify cryptographically verifies that 'sig' is the signed hash of 'data'.
func verify(hash, signature []byte) (bool, error) {
	// recover the public key from the content and the sig
	pk, err := crypto.Ecrecover(hash[:], signature)
	if err != nil {
		return false, errors.Wrap(err, "Failed to verify data")
	}

	// remove recovery id
	sig := signature[:len(signature)-1]
	valid, err := crypto.VerifySignature(pk, hash[:], sig)
	if err != nil {
		return false, err
	}

	fmt.Printf("\nVERIFY - \npk:\t%x\n sig:\t%x\n hash:\t%x\n valid:\t%t\n\n", pk, signature, hash[:], valid)
	return valid, nil
}
