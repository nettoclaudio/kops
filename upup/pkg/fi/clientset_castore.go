/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fi

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"sort"

	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/kops/pkg/apis/kops"
	kopsinternalversion "k8s.io/kops/pkg/client/clientset_generated/clientset/typed/kops/internalversion"
	"k8s.io/kops/pkg/pki"
	"k8s.io/kops/util/pkg/vfs"
)

// ClientsetCAStore is a CAStore implementation that stores keypairs in Keyset on a API server
type ClientsetCAStore struct {
	cluster   *kops.Cluster
	namespace string
	clientset kopsinternalversion.KopsInterface
}

var _ CAStore = &ClientsetCAStore{}
var _ SSHCredentialStore = &ClientsetCAStore{}

// NewClientsetCAStore is the constructor for ClientsetCAStore
func NewClientsetCAStore(cluster *kops.Cluster, clientset kopsinternalversion.KopsInterface, namespace string) CAStore {
	c := &ClientsetCAStore{
		cluster:   cluster,
		clientset: clientset,
		namespace: namespace,
	}

	return c
}

// NewClientsetSSHCredentialStore creates an SSHCredentialStore backed by an API client
func NewClientsetSSHCredentialStore(cluster *kops.Cluster, clientset kopsinternalversion.KopsInterface, namespace string) SSHCredentialStore {
	// Note: currently identical to NewClientsetCAStore
	c := &ClientsetCAStore{
		cluster:   cluster,
		clientset: clientset,
		namespace: namespace,
	}

	return c
}

func parseKeyset(o *kops.Keyset) (*Keyset, error) {
	name := o.Name

	keyset := &Keyset{
		Items: make(map[string]*KeysetItem),
	}

	for _, key := range o.Spec.Keys {
		ki := &KeysetItem{
			Id: key.Id,
		}
		if len(key.PublicMaterial) != 0 {
			cert, err := pki.ParsePEMCertificate(key.PublicMaterial)
			if err != nil {
				klog.Warningf("key public material was %s", key.PublicMaterial)
				return nil, fmt.Errorf("error loading certificate %s/%s: %v", name, key.Id, err)
			}
			ki.Certificate = cert
		}

		if len(key.PrivateMaterial) != 0 {
			privateKey, err := pki.ParsePEMPrivateKey(key.PrivateMaterial)
			if err != nil {
				return nil, fmt.Errorf("error loading private key %s/%s: %v", name, key.Id, err)
			}
			ki.PrivateKey = privateKey
		}

		keyset.Items[key.Id] = ki
	}

	keyset.Primary = keyset.Items[FindPrimary(o).Id]

	return keyset, nil
}

// loadKeyset gets the named Keyset and the format of the Keyset.
func (c *ClientsetCAStore) loadKeyset(ctx context.Context, name string) (*Keyset, error) {
	o, err := c.clientset.Keysets(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("error reading keyset %q: %v", name, err)
	}

	keyset, err := parseKeyset(o)
	if err != nil {
		return nil, err
	}
	return keyset, nil
}

// FindPrimary returns the primary KeysetItem in the Keyset
func FindPrimary(keyset *kops.Keyset) *kops.KeysetItem {
	var primary *kops.KeysetItem
	var primaryVersion *big.Int

	primaryId := keyset.Spec.PrimaryId

	for i := range keyset.Spec.Keys {
		item := &keyset.Spec.Keys[i]
		version, ok := big.NewInt(0).SetString(item.Id, 10)
		if !ok {
			klog.Warningf("Ignoring key item with non-integer version: %q", item.Id)
			continue
		}

		if item.Id == primaryId {
			return item
		}

		if primaryVersion == nil || version.Cmp(primaryVersion) > 0 {
			primary = item
			primaryVersion = version
		}
	}
	return primary
}

// FindPrimaryKeypair implements PKI::FindPrimaryKeypair
func (c *ClientsetCAStore) FindPrimaryKeypair(name string) (*pki.Certificate, *pki.PrivateKey, error) {
	return FindPrimaryKeypair(c, name)
}

// FindKeyset implements CAStore::FindKeyset
func (c *ClientsetCAStore) FindKeyset(name string) (*Keyset, error) {
	ctx := context.TODO()
	return c.loadKeyset(ctx, name)
}

// FindCert implements CAStore::FindCert
func (c *ClientsetCAStore) FindCert(name string) (*pki.Certificate, error) {
	ctx := context.TODO()
	keyset, err := c.loadKeyset(ctx, name)
	if err != nil {
		return nil, err
	}

	if keyset != nil && keyset.Primary != nil {
		return keyset.Primary.Certificate, nil
	}

	return nil, nil
}

// FindCertificatePool implements CAStore::FindCertificatePool
func (c *ClientsetCAStore) FindCertificatePool(name string) (*CertificatePool, error) {
	ctx := context.TODO()
	keyset, err := c.loadKeyset(ctx, name)
	if err != nil {
		return nil, err
	}

	pool := &CertificatePool{}

	if keyset != nil {
		if keyset.Primary != nil {
			pool.Primary = keyset.Primary.Certificate
		}

		for id, item := range keyset.Items {
			if id == keyset.Primary.Id {
				continue
			}
			pool.Secondary = append(pool.Secondary, item.Certificate)
		}
	}
	return pool, nil
}

// FindCertificateKeyset implements CAStore::FindCertificateKeyset
func (c *ClientsetCAStore) FindCertificateKeyset(name string) (*kops.Keyset, error) {
	ctx := context.TODO()
	o, err := c.clientset.Keysets(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("error reading keyset %q: %v", name, err)
	}
	return o, nil
}

// ListKeysets implements CAStore::ListKeysets
func (c *ClientsetCAStore) ListKeysets() ([]*kops.Keyset, error) {
	ctx := context.TODO()
	var items []*kops.Keyset

	{
		list, err := c.clientset.Keysets(c.namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("error listing Keysets: %v", err)
		}

		for i := range list.Items {
			keyset := &list.Items[i]
			switch keyset.Spec.Type {
			case kops.SecretTypeKeypair:
				items = append(items, &list.Items[i])

			case kops.SecretTypeSecret:
				continue // Ignore - this is handled by ClientsetSecretStore
			default:
				return nil, fmt.Errorf("unhandled secret type %q: %v", keyset.Spec.Type, err)
			}
		}
	}

	return items, nil
}

// ListSSHCredentials implements SSHCredentialStore::ListSSHCredentials
func (c *ClientsetCAStore) ListSSHCredentials() ([]*kops.SSHCredential, error) {
	ctx := context.TODO()

	var items []*kops.SSHCredential

	{
		list, err := c.clientset.SSHCredentials(c.namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("error listing SSHCredentials: %v", err)
		}

		for i := range list.Items {
			items = append(items, &list.Items[i])
		}
	}

	return items, nil
}

// StoreKeyset implements CAStore::StoreKeyset
func (c *ClientsetCAStore) StoreKeyset(name string, keyset *Keyset) error {
	ctx := context.TODO()
	return c.storeKeyset(ctx, name, keyset, kops.SecretTypeKeypair)
}

// FindPrivateKey implements CAStore::FindPrivateKey
func (c *ClientsetCAStore) FindPrivateKey(name string) (*pki.PrivateKey, error) {
	ctx := context.TODO()
	keyset, err := c.loadKeyset(ctx, name)
	if err != nil {
		return nil, err
	}

	if keyset != nil && keyset.Primary != nil {
		return keyset.Primary.PrivateKey, nil
	}
	return nil, nil
}

// FindPrivateKeyset implements CAStore::FindPrivateKeyset
func (c *ClientsetCAStore) FindPrivateKeyset(name string) (*kops.Keyset, error) {
	ctx := context.TODO()
	o, err := c.clientset.Keysets(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("error reading keyset %q: %v", name, err)
	}
	return o, nil
}

// storeKeyset saves the specified keyset to the registry.
func (c *ClientsetCAStore) storeKeyset(ctx context.Context, name string, keyset *Keyset, keysetType kops.KeysetType) error {
	create := false
	client := c.clientset.Keysets(c.namespace)
	kopsKeyset, err := client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			kopsKeyset = nil
		} else {
			return fmt.Errorf("error reading keyset %q: %v", name, err)
		}
	}

	if kopsKeyset == nil {
		kopsKeyset = &kops.Keyset{}
		kopsKeyset.Name = name
		kopsKeyset.Spec.Type = keysetType
		create = true
	}

	kopsKeyset.Spec.Keys = nil
	kopsKeyset.Spec.PrimaryId = keyset.Primary.Id

	keys := make([]string, 0, len(keyset.Items))
	for k := range keyset.Items {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return KeysetItemIdOlder(keyset.Items[keys[i]].Id, keyset.Items[keys[j]].Id)
	})

	for _, key := range keys {
		item := keyset.Items[key]
		var publicMaterial bytes.Buffer
		if _, err := item.Certificate.WriteTo(&publicMaterial); err != nil {
			return err
		}

		var privateMaterial bytes.Buffer
		if _, err := item.PrivateKey.WriteTo(&privateMaterial); err != nil {
			return err
		}

		kopsKeyset.Spec.Keys = append(kopsKeyset.Spec.Keys, kops.KeysetItem{
			Id:              item.Id,
			PublicMaterial:  publicMaterial.Bytes(),
			PrivateMaterial: privateMaterial.Bytes(),
		})
	}

	if create {
		if _, err := client.Create(ctx, kopsKeyset, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("error creating keyset %q: %v", name, err)
		}
	} else {
		if _, err := client.Update(ctx, kopsKeyset, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("error updating keyset %q: %v", name, err)
		}
	}
	return nil
}

// deleteKeysetItem deletes the specified key from the registry; deleting the whole Keyset if it was the last one.
func deleteKeysetItem(client kopsinternalversion.KeysetInterface, name string, keysetType kops.KeysetType, id string) error {
	ctx := context.TODO()

	keyset, err := client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("error reading Keyset %q: %v", name, err)
	}

	if keyset.Spec.Type != keysetType {
		return fmt.Errorf("mismatch on Keyset type on %q", name)
	}

	var newKeys []kops.KeysetItem
	found := false
	for _, ki := range keyset.Spec.Keys {
		if ki.Id == id {
			found = true
		} else {
			newKeys = append(newKeys, ki)
		}
	}
	if !found {
		return fmt.Errorf("KeysetItem %q not found in Keyset %q", id, name)
	}
	if len(newKeys) == 0 {
		if err := client.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("error deleting Keyset %q: %v", name, err)
		}
	} else {
		keyset.Spec.Keys = newKeys
		if _, err := client.Update(ctx, keyset, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("error updating Keyset %q: %v", name, err)
		}
	}
	return nil
}

// addSSHCredential saves the specified SSH Credential to the registry, doing an update or insert
func (c *ClientsetCAStore) addSSHCredential(ctx context.Context, name string, publicKey string) error {
	create := false
	client := c.clientset.SSHCredentials(c.namespace)
	sshCredential, err := client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			sshCredential = nil
		} else {
			return fmt.Errorf("error reading SSHCredential %q: %v", name, err)
		}
	}
	if sshCredential == nil {
		sshCredential = &kops.SSHCredential{}
		sshCredential.Name = name
		create = true
	}
	sshCredential.Spec.PublicKey = publicKey
	if create {
		if _, err := client.Create(ctx, sshCredential, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("error creating SSHCredential %q: %v", name, err)
		}
	} else {
		if _, err := client.Update(ctx, sshCredential, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("error updating SSHCredential %q: %v", name, err)
		}
	}
	return nil
}

// deleteSSHCredential deletes the specified SSHCredential from the registry
func (c *ClientsetCAStore) deleteSSHCredential(ctx context.Context, name string) error {
	client := c.clientset.SSHCredentials(c.namespace)
	err := client.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("error deleting SSHCredential %q: %v", name, err)
	}
	return nil
}

// AddSSHPublicKey implements CAStore::AddSSHPublicKey
func (c *ClientsetCAStore) AddSSHPublicKey(name string, pubkey []byte) error {
	ctx := context.TODO()

	_, _, _, _, err := ssh.ParseAuthorizedKey(pubkey)
	if err != nil {
		return fmt.Errorf("error parsing SSH public key: %v", err)
	}

	// TODO: Reintroduce or remove
	//// compute fingerprint to serve as id
	//h := md5.New()
	//_, err = h.Write(sshPublicKey.Marshal())
	//if err != nil {
	//	return err
	//}
	//id = formatFingerprint(h.Sum(nil))

	return c.addSSHCredential(ctx, name, string(pubkey))
}

// FindSSHPublicKeys implements CAStore::FindSSHPublicKeys
func (c *ClientsetCAStore) FindSSHPublicKeys(name string) ([]*kops.SSHCredential, error) {
	ctx := context.TODO()

	o, err := c.clientset.SSHCredentials(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("error reading SSHCredential %q: %v", name, err)
	}

	items := []*kops.SSHCredential{o}
	return items, nil
}

// DeleteKeysetItem implements CAStore::DeleteKeysetItem
func (c *ClientsetCAStore) DeleteKeysetItem(item *kops.Keyset, id string) error {
	switch item.Spec.Type {
	case kops.SecretTypeKeypair:
		client := c.clientset.Keysets(c.namespace)
		return deleteKeysetItem(client, item.Name, kops.SecretTypeKeypair, id)
	default:
		// Primarily because we need to make sure users can recreate them!
		return fmt.Errorf("deletion of keystore items of type %v not (yet) supported", item.Spec.Type)
	}
}

// DeleteSSHCredential implements SSHCredentialStore::DeleteSSHCredential
func (c *ClientsetCAStore) DeleteSSHCredential(item *kops.SSHCredential) error {
	ctx := context.TODO()

	return c.deleteSSHCredential(ctx, item.Name)
}

func (c *ClientsetCAStore) MirrorTo(basedir vfs.Path) error {
	keysets, err := c.ListKeysets()
	if err != nil {
		return err
	}

	for _, keyset := range keysets {
		if err := mirrorKeyset(c.cluster, basedir, keyset); err != nil {
			return err
		}
	}

	sshCredentials, err := c.ListSSHCredentials()
	if err != nil {
		return fmt.Errorf("error listing SSHCredentials: %v", err)
	}

	for _, sshCredential := range sshCredentials {
		if err := mirrorSSHCredential(c.cluster, basedir, sshCredential); err != nil {
			return err
		}
	}

	return nil
}
