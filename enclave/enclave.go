package enclave

import (
	"github.com/kevinburke/nacl"
	"github.com/kevinburke/nacl/box"
	"github.com/kevinburke/nacl/secretbox"
	"gitlab.com/eea/crux/storage"
	"golang.org/x/crypto/sha3"
	"gitlab.com/eea/crux/api"
	log "github.com/sirupsen/logrus"
	"errors"
	"bytes"
)

type Enclave struct {
	Db        storage.DataStore
	PubKeys   []nacl.Key
	PrivKeys  []nacl.Key
	PartyInfo api.PartyInfo
}

func (s *Enclave) Store(
	message *[]byte, sender string, recipients []string) ([]byte, error) {

		var senderPubKey, senderPrivKey nacl.Key

		if sender == "" {
			// from address is either default or specified on communication
			senderPubKey = s.PubKeys[0]
			senderPrivKey = s.PrivKeys[0]
		}

		senderPubKey, err := nacl.Load(sender)
		if err != nil {
			log.WithField("senderPubKey", sender).Errorf(
				"Unable to load sender public key, %v", err)
			return nil, err
		}

		senderPrivKey, err = s.verifySenderKey(senderPubKey)
		if err != nil {
			log.WithField("senderPubKey", sender).Errorf(
				"Unable to locate private key for sender public key, %v", err)
			return nil, err
		}

		return s.store(message, senderPubKey, senderPrivKey, recipients)
	}

func (s *Enclave) store(
	message *[]byte,
	senderPubKey, senderPrivKey nacl.Key,
	recipients []string) ([]byte, error) {

	nonce := nacl.NewNonce()
	masterKey := nacl.NewKey()
	recipientNonce := nacl.NewNonce()

	sealedMessage := secretbox.Seal([]byte{}, *message, nonce, masterKey)

	encryptedPayload := api.EncryptedPayload {
		Sender:         senderPubKey,
		CipherText:     sealedMessage,
		Nonce:          nonce[:],
		RecipientBoxes: make([][]byte, len(recipients)),
		RecipientNonce: recipientNonce,
	}

	for _, recipient := range recipients {
		if url, ok := s.PartyInfo.Recipients[recipient]; ok {

			recipientKey, err := nacl.Load(recipient)
			if err != nil {
				log.WithField("recipientKey", recipientKey).Errorf(
					"Unable to load recipient, %v", err)
			}

			if bytes.Equal((*recipientKey)[:], (*senderPubKey)[:]) {
				log.WithField("recipientKey", recipientKey).Errorf(
					"Sender cannot be recipient, %v", err)
			}

			sealedBox := sealPayload(recipientNonce, masterKey, recipientKey, senderPrivKey)
			encryptedPayload.RecipientBoxes = [][]byte{ sealedBox }
			api.Push(encryptedPayload, url)
		} else {
			log.WithField("recipientKey", recipient).Error("Unable to resolve host")
		}
	}

	sealedBox := sealPayload(recipientNonce, masterKey, senderPubKey, senderPrivKey)
	encryptedPayload.RecipientBoxes = [][]byte{ sealedBox }

	encodedEpl := api.EncodePayload(encryptedPayload)
	return s.storePayload(encryptedPayload, encodedEpl)
}

func (s *Enclave) verifySenderKey(publicKey nacl.Key) (nacl.Key, error) {
	for i, key := range s.PubKeys {
		if bytes.Equal((*publicKey)[:], (*key)[:]) {
			return s.PrivKeys[i], nil
		}
	}
	return nil, errors.New("unable to find private key for public key")
}

func (s *Enclave) StorePayload(encodedEpl []byte) ([]byte, error) {
	decoded := api.DecodePayload(encodedEpl)
	return s.storePayload(decoded, encodedEpl)
}

func (s *Enclave) storePayload(epl api.EncryptedPayload, encodedEpl []byte) ([]byte, error) {

	sha3Hash := sha3.New512()
	sha3Hash.Write(epl.CipherText)
	digestHash := sha3Hash.Sum(nil)

	// We don't store the digest as a base 64 encoded value
	err := s.Db.Write(&digestHash, &encodedEpl)
	return digestHash, err
}

func sealPayload(
	recipientNonce nacl.Nonce,
	masterKey nacl.Key,
	recipientKey nacl.Key,
	privateKey nacl.Key) []byte {

	return box.Seal(
		[]byte{},
		(*masterKey)[:],
		recipientNonce,
		recipientKey,
		privateKey)
}

func (s *Enclave) Retrieve(digestHash *[]byte) ([]byte, error) {

	encodedEpl, err := s.Db.Read(digestHash)
	if err != nil {
		return nil, err
	}

	epl := api.DecodePayload(*encodedEpl)

	masterKey := new([nacl.KeySize]byte)

	_, ok := secretbox.Open(masterKey[:], epl.RecipientBoxes[0], epl.RecipientNonce, s.PrivKeys[0])
	if !ok {
		return nil, errors.New("unable to open master key secret box")
	}

	payload := make([]byte, len(epl.CipherText))
	_, ok = secretbox.Open(nil, epl.CipherText, epl.Nonce, masterKey)
	if !ok {
		return payload, errors.New("unable to open payload secret box")
	}

	return payload, nil
}

func (s *Enclave) Delete(digestHash *[]byte) error {
	return s.Db.Delete(digestHash)
}

func (s *Enclave) UpdatePartyInfo(encoded []byte) {
	pi := api.DecodePartyInfo(encoded)

	for publicKey, url := range pi.Recipients {
		// we should ignore messages about ourselves
		// in order to stop people masquerading as you, there
		// should be a digital signature associated with each
		// url -> node broadcast
		if url != s.PartyInfo.Url {
			s.PartyInfo.Recipients[publicKey] = url
		}
	}

	for url := range pi.Parties {
		// we don't want to broadcast party info to ourselves
		if url != s.PartyInfo.Url {
			s.PartyInfo.Parties[url] = true
		}
	}
}
