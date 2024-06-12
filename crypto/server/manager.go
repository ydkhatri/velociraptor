package server

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"sync"
	"time"

	"github.com/Velocidex/ordereddict"
	"github.com/Velocidex/ttlcache/v2"
	config_proto "www.velocidex.com/golang/velociraptor/config/proto"
	"www.velocidex.com/golang/velociraptor/crypto/client"
	crypto_proto "www.velocidex.com/golang/velociraptor/crypto/proto"
	crypto_utils "www.velocidex.com/golang/velociraptor/crypto/utils"
	"www.velocidex.com/golang/velociraptor/datastore"
	"www.velocidex.com/golang/velociraptor/logging"
	"www.velocidex.com/golang/velociraptor/paths"
	"www.velocidex.com/golang/velociraptor/services/journal"
	"www.velocidex.com/golang/velociraptor/utils"
)

type ServerCryptoManager struct {
	*client.CryptoManager
}

func (self *ServerCryptoManager) AddCertificateRequest(
	config_obj *config_proto.Config,
	csr_pem []byte) (string, error) {
	csr, err := crypto_utils.ParseX509CSRFromPemStr(csr_pem)
	if err != nil {
		return "", err
	}

	if csr.PublicKeyAlgorithm != x509.RSA {
		return "", errors.New("Not RSA algorithm")
	}

	common_name := csr.Subject.CommonName
	public_key := csr.PublicKey.(*rsa.PublicKey)

	// CSRs are always generated by clients and therefore must
	// follow the rules about client id - make sure the client id
	// matches the public key.

	// NOTE: We do not actually sign the CSR at all - since the
	// client is free to generate its own private/public key pair
	// and just uses those to communicate with the server we just
	// store its public key so we can verify its
	// transmissions. The most important thing here is to verfiy
	// that the client id this packet claims to come from
	// corresponds with the public key this client presents. This
	// avoids the possibility of impersonation since the
	// public/private key pair is tied into the client id itself.
	if common_name != crypto_utils.ClientIDFromPublicKey(public_key) {
		return "", errors.New("Invalid CSR")
	}
	err = self.Resolver.SetPublicKey(
		config_obj,
		utils.ClientIdFromConfigObj(common_name, config_obj),
		csr.PublicKey.(*rsa.PublicKey))
	if err != nil {
		return "", err
	}

	// Derive the client id from the common name and the org id
	client_id := utils.ClientIdFromConfigObj(csr.Subject.CommonName, config_obj)
	return client_id, nil
}

func NewServerCryptoManager(
	ctx context.Context,
	config_obj *config_proto.Config,
	wg *sync.WaitGroup) (*ServerCryptoManager, error) {
	if config_obj.Frontend == nil {
		return nil, errors.New("No frontend config")
	}

	cert, err := crypto_utils.ParseX509CertFromPemStr(
		[]byte(config_obj.Frontend.Certificate))
	if err != nil {
		return nil, err
	}

	resolver, err := NewServerPublicKeyResolver(ctx, config_obj, wg)
	if err != nil {
		return nil, err
	}

	base, err := client.NewCryptoManager(config_obj, crypto_utils.GetSubjectName(cert),
		[]byte(config_obj.Frontend.PrivateKey), resolver,
		logging.GetLogger(config_obj, &logging.FrontendComponent))
	if err != nil {
		return nil, err
	}

	server_manager := &ServerCryptoManager{
		CryptoManager: base,
	}

	err = journal.WatchQueueWithCB(ctx, config_obj, wg,
		"Server.Internal.ClientDelete",
		"CryptoServerManager",
		func(ctx context.Context,
			config_obj *config_proto.Config,
			row *ordereddict.Dict) error {

			logger := logging.GetLogger(config_obj, &logging.FrontendComponent)
			logger.Info("Removing client key from cache because client was deleted  %v\n", row)
			client_id, pres := row.GetString("ClientId")
			if pres {
				server_manager.DeleteSubject(client_id)
			}
			return nil
		})

	return server_manager, nil
}

type serverPublicKeyResolver struct {
	// Cache a failure to get the key for a while so we do not get
	// overwhelmed in the slow path for clients that are not yet
	// enrolled.
	negative_lru *ttlcache.Cache
}

func (self *serverPublicKeyResolver) DeleteSubject(client_id string) {
	self.negative_lru.Remove(client_id)
}

func (self *serverPublicKeyResolver) GetPublicKey(
	config_obj *config_proto.Config,
	client_id string) (*rsa.PublicKey, bool) {

	// Check if we failed to get this key recently - this reduces IO
	// while clients enrol.
	_, err := self.negative_lru.Get(client_id)
	if err == nil {
		return nil, false
	}

	client_path_manager := paths.NewClientPathManager(client_id)
	db, err := datastore.GetDB(config_obj)
	if err != nil {
		return nil, false
	}

	pem := &crypto_proto.PublicKey{}
	err = db.GetSubject(config_obj, client_path_manager.Key(), pem)
	if err != nil {
		self.negative_lru.Set(client_id, true)
		return nil, false
	}

	key, err := crypto_utils.PemToPublicKey(pem.Pem)
	if err != nil {
		self.negative_lru.Set(client_id, true)
		return nil, false
	}

	return key, true
}

func (self *serverPublicKeyResolver) SetPublicKey(
	config_obj *config_proto.Config,
	client_id string, key *rsa.PublicKey) error {

	self.negative_lru.Remove(client_id)

	client_path_manager := paths.NewClientPathManager(client_id)
	db, err := datastore.GetDB(config_obj)
	if err != nil {
		return err
	}

	pem := &crypto_proto.PublicKey{
		Pem:        crypto_utils.PublicKeyToPem(key),
		EnrollTime: uint64(time.Now().Unix()),
	}
	return db.SetSubjectWithCompletion(config_obj,
		client_path_manager.Key(), pem, nil)
}

func (self *serverPublicKeyResolver) Clear() {}

func NewServerPublicKeyResolver(
	ctx context.Context,
	config_obj *config_proto.Config,
	wg *sync.WaitGroup) (client.PublicKeyResolver, error) {

	result := &serverPublicKeyResolver{
		// Cache missing keys for 60 seconds.
		negative_lru: ttlcache.NewCache(),
	}

	timeout := time.Duration(10 * time.Second)
	if config_obj.Defaults != nil {
		if config_obj.Defaults.UnauthenticatedLruTimeoutSec < 0 {
			return result, nil
		}

		if config_obj.Defaults.UnauthenticatedLruTimeoutSec > 0 {
			timeout = time.Duration(
				config_obj.Defaults.UnauthenticatedLruTimeoutSec) * time.Second
		}
	}

	result.negative_lru.SetTTL(timeout)
	result.negative_lru.SkipTTLExtensionOnHit(true)

	// Close the LRU when we are done here.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()

		result.negative_lru.Close()
	}()

	return result, nil
}
