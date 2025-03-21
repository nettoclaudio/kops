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
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kops/pkg/acls"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/apis/kops/v1alpha2"
	"k8s.io/kops/pkg/kopscodecs"
	"k8s.io/kops/pkg/pki"
	"k8s.io/kops/pkg/sshcredentials"
	"k8s.io/kops/util/pkg/vfs"
)

type VFSCAStore struct {
	basedir vfs.Path
	cluster *kops.Cluster

	mutex    sync.Mutex
	cachedCA *Keyset
}

var _ CAStore = &VFSCAStore{}
var _ SSHCredentialStore = &VFSCAStore{}

func NewVFSCAStore(cluster *kops.Cluster, basedir vfs.Path) *VFSCAStore {
	c := &VFSCAStore{
		basedir: basedir,
		cluster: cluster,
	}

	return c
}

// NewVFSSSHCredentialStore creates a SSHCredentialStore backed by VFS
func NewVFSSSHCredentialStore(cluster *kops.Cluster, basedir vfs.Path) SSHCredentialStore {
	// Note currently identical to NewVFSCAStore
	c := &VFSCAStore{
		basedir: basedir,
		cluster: cluster,
	}

	return c
}

func (c *VFSCAStore) VFSPath() vfs.Path {
	return c.basedir
}

func (c *VFSCAStore) buildCertificatePoolPath(name string) vfs.Path {
	return c.basedir.Join("issued", name)
}

func (c *VFSCAStore) buildCertificatePath(name string, id string) vfs.Path {
	return c.basedir.Join("issued", name, id+".crt")
}

func (c *VFSCAStore) buildPrivateKeyPoolPath(name string) vfs.Path {
	return c.basedir.Join("private", name)
}

func (c *VFSCAStore) buildPrivateKeyPath(name string, id string) vfs.Path {
	return c.basedir.Join("private", name, id+".key")
}

func (c *VFSCAStore) parseKeysetYaml(data []byte) (*kops.Keyset, bool, error) {
	defaultReadVersion := v1alpha2.SchemeGroupVersion.WithKind("Keyset")

	object, gvk, err := kopscodecs.Decode(data, &defaultReadVersion)
	if err != nil {
		return nil, false, fmt.Errorf("error parsing keyset: %v", err)
	}

	keyset, ok := object.(*kops.Keyset)
	if !ok {
		return nil, false, fmt.Errorf("object was not a keyset, was a %T", object)
	}

	if gvk == nil {
		return nil, false, fmt.Errorf("object did not have GroupVersionKind: %q", keyset.Name)
	}

	return keyset, gvk.Version != keysetFormatLatest, nil
}

// loadKeyset loads a Keyset from the path.
// Returns (nil, nil) if the file is not found
// Bundles avoid the need for a list-files permission, which can be tricky on e.g. GCE
func (c *VFSCAStore) loadKeyset(p vfs.Path) (*Keyset, error) {
	bundlePath := p.Join("keyset.yaml")
	data, err := bundlePath.ReadFile()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("unable to read bundle %q: %v", p, err)
	}

	o, legacyFormat, err := c.parseKeysetYaml(data)
	if err != nil {
		return nil, fmt.Errorf("error parsing bundle %q: %v", p, err)
	}

	keyset, err := parseKeyset(o)
	if err != nil {
		return nil, fmt.Errorf("error mapping bundle %q: %v", p, err)
	}

	keyset.LegacyFormat = legacyFormat
	return keyset, nil
}

func (k *Keyset) ToAPIObject(name string, includePrivateKeyMaterial bool) (*kops.Keyset, error) {
	o := &kops.Keyset{}
	o.Name = name
	o.Spec.Type = kops.SecretTypeKeypair

	keys := make([]string, 0, len(k.Items))
	for k := range k.Items {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return KeysetItemIdOlder(k.Items[keys[i]].Id, k.Items[keys[j]].Id)
	})

	for _, key := range keys {
		ki := k.Items[key]
		oki := kops.KeysetItem{
			Id: ki.Id,
		}

		if ki.Certificate != nil {
			var publicMaterial bytes.Buffer
			if _, err := ki.Certificate.WriteTo(&publicMaterial); err != nil {
				return nil, err
			}
			oki.PublicMaterial = publicMaterial.Bytes()
		}

		if includePrivateKeyMaterial && ki.PrivateKey != nil {
			var privateMaterial bytes.Buffer
			if _, err := ki.PrivateKey.WriteTo(&privateMaterial); err != nil {
				return nil, err
			}

			oki.PrivateMaterial = privateMaterial.Bytes()
		}

		o.Spec.Keys = append(o.Spec.Keys, oki)
	}
	if k.Primary != nil {
		o.Spec.PrimaryId = k.Primary.Id
	}
	return o, nil
}

// writeKeysetBundle writes a Keyset bundle to VFS.
func (c *VFSCAStore) writeKeysetBundle(p vfs.Path, name string, keyset *Keyset, includePrivateKeyMaterial bool) error {
	p = p.Join("keyset.yaml")

	o, err := keyset.ToAPIObject(name, includePrivateKeyMaterial)
	if err != nil {
		return err
	}

	objectData, err := serializeKeysetBundle(o)
	if err != nil {
		return err
	}

	acl, err := acls.GetACL(p, c.cluster)
	if err != nil {
		return err
	}
	return p.WriteFile(bytes.NewReader(objectData), acl)
}

// serializeKeysetBundle converts a Keyset bundle to yaml, for writing to VFS.
func serializeKeysetBundle(o *kops.Keyset) ([]byte, error) {
	var objectData bytes.Buffer
	codecs := kopscodecs.Codecs
	yaml, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), "application/yaml")
	if !ok {
		klog.Fatalf("no YAML serializer registered")
	}
	encoder := codecs.EncoderForVersion(yaml.Serializer, v1alpha2.SchemeGroupVersion)

	if err := encoder.Encode(o, &objectData); err != nil {
		return nil, fmt.Errorf("error serializing keyset: %v", err)
	}
	return objectData.Bytes(), nil
}

// removePrivateKeyMaterial returns a copy of the Keyset with the private key data removed
func removePrivateKeyMaterial(o *kops.Keyset) *kops.Keyset {
	c := o.DeepCopy()

	for i := range c.Spec.Keys {
		c.Spec.Keys[i].PrivateMaterial = nil
	}

	return c
}

func (c *VFSCAStore) FindPrimaryKeypair(name string) (*pki.Certificate, *pki.PrivateKey, error) {
	return FindPrimaryKeypair(c, name)
}

func (c *VFSCAStore) FindKeyset(id string) (*Keyset, error) {
	certs, err := c.loadKeyset(c.buildCertificatePoolPath(id))

	if (certs == nil || os.IsNotExist(err)) && id == "service-account" {
		// The strange name is because Kops prior to 1.19 used the api-server TLS key for this.
		id = "master"
		certs, err = c.loadKeyset(c.buildCertificatePoolPath(id))
		if certs != nil {
			certs.LegacyFormat = true
		}
	}

	if err != nil {
		return nil, err
	}

	keys, err := c.findPrivateKeyset(id)
	if err != nil {
		return nil, err
	}

	if certs != nil {
		if keys == nil {
			return certs, nil
		}
		if certs.LegacyFormat {
			keys.LegacyFormat = true
		}
		for key, certItem := range certs.Items {
			keyItem := keys.Items[key]
			if keyItem == nil {
				keys.Items[key] = certItem
			} else if keyItem.Certificate == nil {
				keyItem.Certificate = certItem.Certificate
			}
		}
	}

	return keys, nil
}

func (c *VFSCAStore) findCert(name string) (*pki.Certificate, bool, error) {
	p := c.buildCertificatePoolPath(name)
	certs, err := c.loadKeyset(p)
	if err != nil {
		return nil, false, fmt.Errorf("error in 'FindCert' attempting to load cert %q: %v", name, err)
	}

	if certs != nil && certs.Primary != nil {
		return certs.Primary.Certificate, certs.LegacyFormat, nil
	}

	return nil, false, nil
}

func (c *VFSCAStore) FindCert(name string) (*pki.Certificate, error) {
	cert, _, err := c.findCert(name)
	return cert, err
}

func (c *VFSCAStore) FindCertificatePool(name string) (*CertificatePool, error) {
	var certs *Keyset

	var err error
	p := c.buildCertificatePoolPath(name)
	certs, err = c.loadKeyset(p)
	if err != nil {
		return nil, fmt.Errorf("error in 'FindCertificatePool' attempting to load cert %q: %v", name, err)
	}

	pool := &CertificatePool{}

	if certs != nil {
		if certs.Primary != nil {
			pool.Primary = certs.Primary.Certificate
		}

		for k, cert := range certs.Items {
			if certs.Primary != nil && k == certs.Primary.Id {
				continue
			}
			if cert.Certificate == nil {
				continue
			}
			pool.Secondary = append(pool.Secondary, cert.Certificate)
		}
	}
	return pool, nil
}

func (c *VFSCAStore) FindCertificateKeyset(name string) (*kops.Keyset, error) {
	p := c.buildCertificatePoolPath(name)
	certs, err := c.loadKeyset(p)
	if err != nil {
		return nil, fmt.Errorf("error in 'FindCertificatePool' attempting to load cert %q: %v", name, err)
	}

	if certs == nil {
		return nil, nil
	}

	o, err := certs.ToAPIObject(name, false)
	if err != nil {
		return nil, err
	}

	return o, nil
}

// ListKeysets implements CAStore::ListKeysets
func (c *VFSCAStore) ListKeysets() ([]*kops.Keyset, error) {
	keysets := make(map[string]*kops.Keyset)

	{
		baseDir := c.basedir.Join("issued")
		files, err := baseDir.ReadTree()
		if err != nil {
			return nil, fmt.Errorf("error reading directory %q: %v", baseDir, err)
		}

		for _, f := range files {
			relativePath, err := vfs.RelativePath(baseDir, f)
			if err != nil {
				return nil, err
			}

			tokens := strings.Split(relativePath, "/")
			if len(tokens) != 2 {
				klog.V(2).Infof("ignoring unexpected file in keystore: %q", f)
				continue
			}

			name := tokens[0]
			keyset := keysets[name]
			if keyset == nil {
				keyset = &kops.Keyset{}
				keyset.Name = tokens[0]
				keyset.Spec.Type = kops.SecretTypeKeypair
				keysets[name] = keyset
			}

			if tokens[1] == "keyset.yaml" {
				// TODO: Should we load the keyset to get the actual ids?
			} else {
				keyset.Spec.Keys = append(keyset.Spec.Keys, kops.KeysetItem{
					Id: strings.TrimSuffix(tokens[1], ".crt"),
				})
			}
		}
	}

	var items []*kops.Keyset
	for _, v := range keysets {
		items = append(items, v)
	}
	return items, nil
}

// ListSSHCredentials implements SSHCredentialStore::ListSSHCredentials
func (c *VFSCAStore) ListSSHCredentials() ([]*kops.SSHCredential, error) {
	var items []*kops.SSHCredential

	{
		baseDir := c.basedir.Join("ssh", "public")
		files, err := baseDir.ReadTree()
		if err != nil {
			return nil, fmt.Errorf("error reading directory %q: %v", baseDir, err)
		}

		for _, f := range files {
			relativePath, err := vfs.RelativePath(baseDir, f)
			if err != nil {
				return nil, err
			}

			tokens := strings.Split(relativePath, "/")
			if len(tokens) != 2 {
				klog.V(2).Infof("ignoring unexpected file in keystore: %q", f)
				continue
			}

			pubkey, err := f.ReadFile()
			if err != nil {
				return nil, fmt.Errorf("error reading SSH credential %q: %v", f, err)
			}

			item := &kops.SSHCredential{}
			item.Name = tokens[0]
			item.Spec.PublicKey = string(pubkey)
			items = append(items, item)
		}
	}

	return items, nil
}

// MirrorTo will copy keys to a vfs.Path, which is often easier for a machine to read
func (c *VFSCAStore) MirrorTo(basedir vfs.Path) error {
	if basedir.Path() == c.basedir.Path() {
		klog.V(2).Infof("Skipping key store mirror from %q to %q (same paths)", c.basedir, basedir)
		return nil
	}
	klog.V(2).Infof("Mirroring key store from %q to %q", c.basedir, basedir)

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

// mirrorKeyset writes Keyset bundles for the certificates & privatekeys.
func mirrorKeyset(cluster *kops.Cluster, basedir vfs.Path, keyset *kops.Keyset) error {
	primary := FindPrimary(keyset)
	if primary == nil {
		return fmt.Errorf("found keyset with no primary data: %s", keyset.Name)
	}

	switch keyset.Spec.Type {
	case kops.SecretTypeKeypair:
		{
			data, err := serializeKeysetBundle(removePrivateKeyMaterial(keyset))
			if err != nil {
				return err
			}
			p := basedir.Join("issued", keyset.Name, "keyset.yaml")
			acl, err := acls.GetACL(p, cluster)
			if err != nil {
				return err
			}

			err = p.WriteFile(bytes.NewReader(data), acl)
			if err != nil {
				return fmt.Errorf("error writing %q: %v", p, err)
			}
		}

		{
			data, err := serializeKeysetBundle(keyset)
			if err != nil {
				return err
			}
			p := basedir.Join("private", keyset.Name, "keyset.yaml")
			acl, err := acls.GetACL(p, cluster)
			if err != nil {
				return err
			}

			err = p.WriteFile(bytes.NewReader(data), acl)
			if err != nil {
				return fmt.Errorf("error writing %q: %v", p, err)
			}
		}

	default:
		return fmt.Errorf("unknown secret type: %q", keyset.Spec.Type)
	}

	return nil
}

// mirrorSSHCredential writes the SSH credential file to the mirror location
func mirrorSSHCredential(cluster *kops.Cluster, basedir vfs.Path, sshCredential *kops.SSHCredential) error {
	id, err := sshcredentials.Fingerprint(sshCredential.Spec.PublicKey)
	if err != nil {
		return fmt.Errorf("error fingerprinting SSH public key %q: %v", sshCredential.Name, err)
	}

	p := basedir.Join("ssh", "public", sshCredential.Name, id)
	acl, err := acls.GetACL(p, cluster)
	if err != nil {
		return err
	}

	err = p.WriteFile(bytes.NewReader([]byte(sshCredential.Spec.PublicKey)), acl)
	if err != nil {
		return fmt.Errorf("error writing %q: %v", p, err)
	}

	return nil
}

func (c *VFSCAStore) StoreKeyset(name string, keyset *Keyset) error {
	{
		p := c.buildPrivateKeyPoolPath(name)
		if err := c.writeKeysetBundle(p, name, keyset, true); err != nil {
			return fmt.Errorf("writing private bundle: %v", err)
		}
	}

	{
		p := c.buildCertificatePoolPath(name)
		if err := c.writeKeysetBundle(p, name, keyset, false); err != nil {
			return fmt.Errorf("writing certificate bundle: %v", err)
		}
	}

	return nil
}

func (c *VFSCAStore) findPrivateKeyset(id string) (*Keyset, error) {
	var keys *Keyset
	var err error
	if id == CertificateIDCA {
		c.mutex.Lock()
		defer c.mutex.Unlock()

		cached := c.cachedCA
		if cached != nil {
			return cached, nil
		}

		keys, err = c.loadKeyset(c.buildPrivateKeyPoolPath(id))
		if err != nil {
			return nil, err
		}

		if keys == nil {
			klog.Warningf("CA private key was not found")
			// We no longer generate CA certificates automatically - too race-prone
		} else {
			c.cachedCA = keys
		}
	} else {
		p := c.buildPrivateKeyPoolPath(id)
		keys, err = c.loadKeyset(p)
		if err != nil {
			return nil, err
		}
	}

	return keys, nil
}

func (c *VFSCAStore) FindPrivateKey(id string) (*pki.PrivateKey, error) {
	keys, err := c.findPrivateKeyset(id)
	if err != nil {
		return nil, err
	}

	var key *pki.PrivateKey
	if keys != nil && keys.Primary != nil {
		key = keys.Primary.PrivateKey
	}
	return key, nil
}

func (c *VFSCAStore) FindPrivateKeyset(name string) (*kops.Keyset, error) {
	keys, err := c.findPrivateKeyset(name)
	if err != nil {
		return nil, err
	}

	o, err := keys.ToAPIObject(name, true)
	if err != nil {
		return nil, err
	}

	return o, nil
}

func (c *VFSCAStore) deletePrivateKey(name string, id string) (bool, error) {
	// Delete the file itself
	{

		p := c.buildPrivateKeyPath(name, id)
		if err := p.Remove(); err != nil && !os.IsNotExist(err) {
			return false, err
		}
	}

	// Update the bundle
	{
		p := c.buildPrivateKeyPoolPath(name)
		ks, err := c.loadKeyset(p)
		if err != nil {
			return false, err
		}

		if ks == nil || ks.Items[id] == nil {
			return false, nil
		}
		delete(ks.Items, id)
		if ks.Primary != nil && ks.Primary.Id == id {
			ks.Primary = nil
		}

		if err := c.writeKeysetBundle(p, name, ks, true); err != nil {
			return false, fmt.Errorf("error writing bundle: %v", err)
		}
	}

	return true, nil
}

func (c *VFSCAStore) deleteCertificate(name string, id string) (bool, error) {
	// Delete the file itself
	{
		p := c.buildCertificatePath(name, id)
		if err := p.Remove(); err != nil && !os.IsNotExist(err) {
			return false, err
		}
	}

	// Update the bundle
	{
		p := c.buildCertificatePoolPath(name)
		ks, err := c.loadKeyset(p)
		if err != nil {
			return false, err
		}

		if ks == nil || ks.Items[id] == nil {
			return false, nil
		}
		delete(ks.Items, id)
		if ks.Primary != nil && ks.Primary.Id == id {
			ks.Primary = nil
		}

		if err := c.writeKeysetBundle(p, name, ks, false); err != nil {
			return false, fmt.Errorf("error writing bundle: %v", err)
		}
	}

	return true, nil
}

// AddSSHPublicKey stores an SSH public key
func (c *VFSCAStore) AddSSHPublicKey(name string, pubkey []byte) error {
	id, err := sshcredentials.Fingerprint(string(pubkey))
	if err != nil {
		return fmt.Errorf("error fingerprinting SSH public key %q: %v", name, err)
	}

	p := c.buildSSHPublicKeyPath(name, id)

	acl, err := acls.GetACL(p, c.cluster)
	if err != nil {
		return err
	}

	return p.WriteFile(bytes.NewReader(pubkey), acl)
}

func (c *VFSCAStore) buildSSHPublicKeyPath(name string, id string) vfs.Path {
	// id is fingerprint with colons, but we store without colons
	id = strings.Replace(id, ":", "", -1)
	return c.basedir.Join("ssh", "public", name, id)
}

func (c *VFSCAStore) FindSSHPublicKeys(name string) ([]*kops.SSHCredential, error) {
	p := c.basedir.Join("ssh", "public", name)

	files, err := p.ReadDir()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var items []*kops.SSHCredential

	for _, f := range files {
		data, err := f.ReadFile()
		if err != nil {
			if os.IsNotExist(err) {
				klog.V(2).Infof("Ignoring not-found issue reading %q", f)
				continue
			}
			return nil, fmt.Errorf("error loading SSH item %q: %v", f, err)
		}

		item := &kops.SSHCredential{}
		item.Name = name
		item.Spec.PublicKey = string(data)
		items = append(items, item)
	}

	return items, nil
}

// DeleteKeysetItem implements CAStore::DeleteKeysetItem
func (c *VFSCAStore) DeleteKeysetItem(item *kops.Keyset, id string) error {
	switch item.Spec.Type {
	case kops.SecretTypeKeypair:
		_, ok := big.NewInt(0).SetString(id, 10)
		if !ok {
			return fmt.Errorf("keypair had non-integer version: %q", id)
		}
		removed, err := c.deleteCertificate(item.Name, id)
		if err != nil {
			return fmt.Errorf("error deleting certificate: %v", err)
		}
		if !removed {
			klog.Warningf("certificate %s:%s was not found", item.Name, id)
		}
		removed, err = c.deletePrivateKey(item.Name, id)
		if err != nil {
			return fmt.Errorf("error deleting private key: %v", err)
		}
		if !removed {
			klog.Warningf("private key %s:%s was not found", item.Name, id)
		}
		return nil

	default:
		// Primarily because we need to make sure users can recreate them!
		return fmt.Errorf("deletion of keystore items of type %v not (yet) supported", item.Spec.Type)
	}
}

func (c *VFSCAStore) DeleteSSHCredential(item *kops.SSHCredential) error {
	if item.Spec.PublicKey == "" {
		return fmt.Errorf("must specific public key to delete SSHCredential")
	}
	id, err := sshcredentials.Fingerprint(item.Spec.PublicKey)
	if err != nil {
		return fmt.Errorf("invalid PublicKey when deleting SSHCredential: %v", err)
	}
	p := c.buildSSHPublicKeyPath(item.Name, id)
	return p.Remove()
}
