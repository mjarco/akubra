package crdstore

import (
	"fmt"
	"net/http"
	"time"

	"errors"

	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/allegro/akubra/log"
	"github.com/levigross/grequests"
	"golang.org/x/sync/syncmap"
)

const (
	keyPattern                   = "%s_____%s"
	requestOptionsDialTimeout    = 50 * time.Millisecond
	requestOptionsRequestTimeout = 100 * time.Millisecond
	refreshTTLPercent            = 80               // Background refresh after refreshTTLPercent*TTL
	ttl                          = 10 * time.Second // Cache TTL
)

// ErrCredentialsNotFound - Credential for given accessKey and backend haven't been found in yaml file
var ErrCredentialsNotFound = errors.New("credentials not found")

// CredentialStore instance
var instances map[string]*CredentialsStore

// CredentialsStore - gets a caches credentials from akubra-crdstore
type CredentialsStore struct {
	endpoint string
	cache    *syncmap.Map
	TTL      time.Duration
	lock     sync.Mutex
}

// GetInstance - Get crdstore instance for endpoint
func GetInstance(endpoint string) *CredentialsStore {
	if instances == nil {
		instances = make(map[string]*CredentialsStore)
	}

	if instance, ok := instances[endpoint]; ok {
		return instance
	}

	instances[endpoint] = initializeCredentialsStore(endpoint)
	return instances[endpoint]
}

// InitializeCredentialsStore - Constructor for CredentialsStore
func initializeCredentialsStore(endpoint string) *CredentialsStore {
	instances[endpoint] = &CredentialsStore{
		endpoint: endpoint,
		cache:    new(syncmap.Map),
		TTL:      ttl,
	}
	return instances[endpoint]
}

func (cs *CredentialsStore) prepareKey(accessKey, backend string) string {
	return fmt.Sprintf(keyPattern, accessKey, backend)
}

func (cs *CredentialsStore) updateCache(accessKey, backend, key string, csd *CredentialsStoreData, blocking bool) (newCsd *CredentialsStoreData, err error) {
	if !blocking {
		if !cs.tryLock() {
			return csd, nil
		}
	} else {
		cs.lock.Lock()
	}
	newCsd, err = cs.GetFromService(cs.endpoint, accessKey, backend)
	switch {
	case err == nil:
		newCsd.err = nil
	case err == ErrCredentialsNotFound:
		newCsd = &CredentialsStoreData{EOL: time.Now().Add(cs.TTL), err: ErrCredentialsNotFound}
	default:
		if csd == nil {
			newCsd = &CredentialsStoreData{EOL: time.Now().Add(cs.TTL), err: err}
		} else {
			*newCsd = *csd
		}
		newCsd.err = err
		log.Printf("Error while updating cache for key `%s`: `%s`", key, err)
	}
	newCsd.EOL = time.Now().Add(cs.TTL)
	cs.cache.Store(key, newCsd)
	cs.lock.Unlock()
	if newCsd.AccessKey == "" {
		return nil, newCsd.err
	}
	return newCsd, nil
}

func (cs *CredentialsStore) tryLock() bool {
	return atomic.CompareAndSwapInt32((*int32)(unsafe.Pointer(&cs.lock)), 0, 1)
}

// Get - Gets key from cache or from akubra-crdstore if TTL has expired
func (cs *CredentialsStore) Get(accessKey, backend string) (csd *CredentialsStoreData, err error) {
	key := cs.prepareKey(accessKey, backend)

	if value, ok := cs.cache.Load(key); ok {
		csd = value.(*CredentialsStoreData)
	}

	switch {
	case csd == nil || csd.AccessKey == "":
		return cs.updateCache(accessKey, backend, key, csd, true)
	case time.Now().After(csd.EOL):
		return cs.updateCache(accessKey, backend, key, csd, false)
	case time.Now().Add(cs.TTL / time.Second * (100 - refreshTTLPercent) * 10 * time.Millisecond).After(csd.EOL):
		go cs.updateCache(accessKey, backend, key, csd, false)
	}

	return
}

// GetFromService - Get Credential akubra-crdstore service
func (cs *CredentialsStore) GetFromService(endpoint, accessKey, backend string) (csd *CredentialsStoreData, err error) {
	csd = &CredentialsStoreData{}
	ro := &grequests.RequestOptions{
		DialTimeout:    requestOptionsDialTimeout,
		RequestTimeout: requestOptionsRequestTimeout,
		RedirectLimit:  1,
		IsAjax:         false,
	}
	resp, err := grequests.Get(fmt.Sprintf(urlPattern, endpoint, accessKey, backend), ro)
	switch {
	case err != nil:
		return csd, fmt.Errorf("unable to make request to credentials store service - err: %s", err)
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrCredentialsNotFound
	case resp.StatusCode != http.StatusOK:
		return csd, fmt.Errorf("unable to get credentials from store service - StatusCode: %d", resp.StatusCode)
	}

	credentials := resp.String()
	if len(credentials) == 0 {
		return csd, fmt.Errorf("got empty credentials from store service%s", "")
	}

	err = csd.Unmarshal(credentials)

	return
}
