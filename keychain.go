// +build darwin,cgo

package keyring

import (
	"errors"
	"fmt"
	"log"
	"os"

	gokeychain "github.com/keybase/go-keychain"
	touchid "github.com/lox/go-touchid"
	"golang.org/x/crypto/ssh/terminal"
)

const (
	biometricsAccount = "x"
	biometricsService = "x"
	biometricsLabel   = "Passphrase for %s"
)

type keychain struct {
	path          string
	service       string
	passphrase    string
	authenticated bool

	passwordFunc PromptFunc

	isSynchronizable         bool
	isAccessibleWhenUnlocked bool
	isTrusted                bool
}

func init() {
	supportedBackends[KeychainBackend] = opener(func(cfg Config) (Keyring, error) {
		kc := &keychain{
			service:      cfg.ServiceName,
			passwordFunc: cfg.KeychainPasswordFunc,

			// Set the isAccessibleWhenUnlocked to the boolean value of
			// KeychainAccessibleWhenUnlocked is a shorthand for setting the accessibility value.
			// See: https://developer.apple.com/documentation/security/ksecattraccessiblewhenunlocked
			isAccessibleWhenUnlocked: cfg.KeychainAccessibleWhenUnlocked,
		}
		if cfg.KeychainName != "" {
			kc.path = cfg.KeychainName + ".keychain"
		}
		if cfg.KeychainTrustApplication {
			kc.isTrusted = true
		}
		return kc, nil
	})
}

func (k *keychain) Get(key string) (Item, error) {
	query := gokeychain.NewItem()
	query.SetSecClass(gokeychain.SecClassGenericPassword)
	query.SetService(k.service)
	query.SetAccount(key)
	query.SetMatchLimit(gokeychain.MatchLimitOne)
	query.SetReturnAttributes(true)
	query.SetReturnData(true)

	if k.path != "" {
		// When we are querying, we don't create by default
		query.SetMatchSearchList(gokeychain.NewWithPath(k.path))
	}

	debugf("Querying keychain for service=%q, account=%q, keychain=%q", k.service, key, k.path)
	results, err := gokeychain.QueryItem(query)
	if err == gokeychain.ErrorItemNotFound || len(results) == 0 {
		debugf("No results found")
		return Item{}, ErrKeyNotFound
	}

	if err != nil {
		debugf("Error: %#v", err)
		return Item{}, err
	}

	item := Item{
		Key:         key,
		Data:        results[0].Data,
		Label:       results[0].Label,
		Description: results[0].Description,
	}

	debugf("Found item %q", results[0].Label)
	return item, nil
}

func (k *keychain) GetMetadata(key string) (Metadata, error) {
	query := gokeychain.NewItem()
	query.SetSecClass(gokeychain.SecClassGenericPassword)
	query.SetService(k.service)
	query.SetAccount(key)
	query.SetMatchLimit(gokeychain.MatchLimitOne)
	query.SetReturnAttributes(true)
	query.SetReturnData(false)
	query.SetReturnRef(true)

	debugf("Querying keychain for metadata of service=%q, account=%q, keychain=%q", k.service, key, k.path)
	results, err := gokeychain.QueryItem(query)
	if err == gokeychain.ErrorItemNotFound || len(results) == 0 {
		debugf("No results found")
		return Metadata{}, ErrKeyNotFound
	} else if err != nil {
		debugf("Error: %#v", err)
		return Metadata{}, err
	}

	md := Metadata{
		Item: &Item{
			Key:         key,
			Label:       results[0].Label,
			Description: results[0].Description,
		},
		ModificationTime: results[0].ModificationDate,
	}

	debugf("Found metadata for %q", md.Item.Label)

	return md, nil
}

func (k *keychain) Set(item Item) error {
	var kc gokeychain.Keychain

	// when we are setting a value, we create or open
	if k.path != "" {
		var err error
		kc, err = k.createOrOpen()
		if err != nil {
			return err
		}
	}

	kcItem := gokeychain.NewItem()
	kcItem.SetSecClass(gokeychain.SecClassGenericPassword)
	kcItem.SetService(k.service)
	kcItem.SetAccount(item.Key)
	kcItem.SetLabel(item.Label)
	kcItem.SetDescription(item.Description)
	kcItem.SetData(item.Data)

	if k.path != "" {
		kcItem.UseKeychain(kc)
	}

	if k.isSynchronizable && !item.KeychainNotSynchronizable {
		kcItem.SetSynchronizable(gokeychain.SynchronizableYes)
	}

	if k.isAccessibleWhenUnlocked {
		kcItem.SetAccessible(gokeychain.AccessibleWhenUnlocked)
	}

	isTrusted := k.isTrusted && !item.KeychainNotTrustApplication

	if isTrusted {
		debugf("Keychain item trusts keyring")
		kcItem.SetAccess(&gokeychain.Access{
			Label:               item.Label,
			TrustedApplications: nil,
		})
	} else {
		debugf("Keychain item doesn't trust keyring")
		kcItem.SetAccess(&gokeychain.Access{
			Label:               item.Label,
			TrustedApplications: []string{},
		})
	}

	debugf("Adding service=%q, label=%q, account=%q, trusted=%v to osx keychain %q", k.service, item.Label, item.Key, isTrusted, k.path)

	if err := gokeychain.AddItem(kcItem); err == gokeychain.ErrorDuplicateItem {
		debugf("Item already exists, updating")
		queryItem := gokeychain.NewItem()
		queryItem.SetSecClass(gokeychain.SecClassGenericPassword)
		queryItem.SetService(k.service)
		queryItem.SetAccount(item.Key)
		queryItem.SetMatchLimit(gokeychain.MatchLimitOne)
		queryItem.SetReturnAttributes(true)

		if k.path != "" {
			queryItem.SetMatchSearchList(kc)
		}

		results, err := gokeychain.QueryItem(queryItem)
		if err != nil {
			return fmt.Errorf("Failed to query keychain: %v", err)
		}
		if len(results) == 0 {
			return errors.New("no results")
		}

		// Don't call SetAccess() as this will cause multiple prompts on update, even when we are not updating the AccessList
		kcItem.SetAccess(nil)

		if err := gokeychain.UpdateItem(queryItem, kcItem); err != nil {
			return fmt.Errorf("Failed to update item in keychain: %v", err)
		}
	}

	return nil
}

func (k *keychain) Remove(key string) error {
	item := gokeychain.NewItem()
	item.SetSecClass(gokeychain.SecClassGenericPassword)
	item.SetService(k.service)
	item.SetAccount(key)

	if k.path != "" {
		kc := gokeychain.NewWithPath(k.path)

		if err := kc.Status(); err != nil {
			if err == gokeychain.ErrorNoSuchKeychain {
				return ErrKeyNotFound
			}
			return err
		}

		item.SetMatchSearchList(kc)
	}

	debugf("Removing keychain item service=%q, account=%q, keychain %q", k.service, key, k.path)
	return gokeychain.DeleteItem(item)
}

func (k *keychain) Keys() ([]string, error) {
	query := gokeychain.NewItem()
	query.SetSecClass(gokeychain.SecClassGenericPassword)
	query.SetService(k.service)
	query.SetMatchLimit(gokeychain.MatchLimitAll)
	query.SetReturnAttributes(true)

	if k.path != "" {
		kc := gokeychain.NewWithPath(k.path)

		if err := kc.Status(); err != nil {
			if err == gokeychain.ErrorNoSuchKeychain {
				return []string{}, nil
			}
			return nil, err
		}

		query.SetMatchSearchList(kc)
	}

	debugf("Querying keychain for service=%q, keychain=%q", k.service, k.path)
	results, err := gokeychain.QueryItem(query)
	if err != nil {
		return nil, err
	}

	debugf("Found %d results", len(results))
	accountNames := make([]string, len(results))
	for idx, r := range results {
		accountNames[idx] = r.Account
	}

	return accountNames, nil
}

func (k *keychain) setupBiometrics() error {
	fmt.Println("\nTo use biometrics for authentication, your keychain password needs to be stored in your login keychain.\n" +
		"You will be prompted for your password.\n")

	fmt.Printf("Password for %q: ", k.path)
	passphrase, err := terminal.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}

	fmt.Println()

	// needs to be locked first in-case it's already unlocked. if so, an incorrect password can be stored
	log.Printf("Locking keychain %s", k.path)
	gokeychain.LockAtPath(k.path)

	log.Printf("Unlocking keychain %s", k.path)
	if err := gokeychain.UnlockAtPath(k.path, string(passphrase)); err != nil {
		return err
	}

	k.passphrase = string(passphrase)

	item := gokeychain.NewItem()
	item.SetSecClass(gokeychain.SecClassGenericPassword)
	item.SetService(biometricsService)
	item.SetAccount(biometricsAccount)
	item.SetLabel(fmt.Sprintf(biometricsLabel, k.path))
	item.SetData(passphrase)
	item.SetSynchronizable(gokeychain.SynchronizableNo)
	item.SetAccessible(gokeychain.AccessibleWhenUnlocked)

	log.Printf("Adding service=%q, account=%q to osx keychain %s", biometricsService, biometricsAccount, k.path)
	return gokeychain.AddItem(item)
}

func (k *keychain) openWithBiometrics() (gokeychain.Keychain, error) {
	if !k.authenticated {
		log.Printf("Checking touchid")
		ok, err := touchid.Authenticate("unlock " + k.path)
		if !ok || err != nil {
			return gokeychain.Keychain{}, errors.New("Authentication with biometrics failed")
		}

		k.authenticated = true

		log.Printf("Looking up password stored in login.keychain")
		query := gokeychain.NewItem()
		query.SetSecClass(gokeychain.SecClassGenericPassword)
		query.SetService(biometricsService)
		query.SetAccount(biometricsAccount)
		query.SetLabel(fmt.Sprintf(biometricsLabel, k.path))
		query.SetMatchLimit(gokeychain.MatchLimitOne)
		query.SetReturnData(true)

		results, err := gokeychain.QueryItem(query)
		if err != nil {
			return gokeychain.Keychain{}, err
		}

		if len(results) != 1 {
			err := k.setupBiometrics()
			if err != nil {
				return gokeychain.Keychain{}, err
			}
		} else {
			log.Printf("Found passphrase in login.keychain, unlocking %s with stored password", k.path)
			if err = gokeychain.UnlockAtPath(k.path, string(results[0].Data)); err != nil {
				return gokeychain.Keychain{}, err
			}
			k.passphrase = string(results[0].Data)
		}
	}

	return gokeychain.NewWithPath(k.path), nil
}

func (k *keychain) createOrOpen() (gokeychain.Keychain, error) {
	kc := gokeychain.NewWithPath(k.path)

	debugf("Checking keychain status")
	err := kc.Status()
	if err == nil {
		debugf("Keychain status returned nil, keychain exists")
		log.Printf("Opening %s with biometrics", k.path)
		return k.openWithBiometrics()
		//return kc, nil
	}

	debugf("Keychain status returned error: %v", err)

	if err != gokeychain.ErrorNoSuchKeychain {
		return gokeychain.Keychain{}, err
	}

	if k.passwordFunc == nil {
		debugf("Creating keychain %s with prompt", k.path)
		return gokeychain.NewKeychainWithPrompt(k.path)
	}

	passphrase, err := k.passwordFunc("Enter passphrase for keychain")
	if err != nil {
		return gokeychain.Keychain{}, err
	}

	debugf("Creating keychain %s with provided password", k.path)
	return gokeychain.NewKeychain(k.path, passphrase)
}
