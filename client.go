package knox

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sync"
	"time"
)

const refresh = 10 * time.Second

// For linear random backoff on write requests.
const baseBackoff = 50 * time.Millisecond
const maxBackoff = 3 * time.Second
const maxRetryAttempts = 3

// Client is an interface for interacting with a specific knox key
type Client interface {
	// GetPrimary returns the primary key version for the knox key.
	// This should be used for sending relationships like signing, encrypting, or api secrets
	GetPrimary() string
	// GetActive returns all of the active key versions for the knox key.
	// This should be used for receiving relationships like verifying or decrypting.
	GetActive() []string
	// GetKeyObject returns the full key object, including versions, ACLs, and other attributes.
	GetKeyObject() Key
}

type fileClient struct {
	sync.RWMutex
	keyID     string
	primary   string
	active    []string
	keyObject Key
}

// update reads the file from a specific location, decodes json, and updates the key in memory.
func (c *fileClient) update() error {
	var key Key
	f, err := os.Open("/var/lib/knox/v0/keys/" + c.keyID)
	if err != nil {
		return fmt.Errorf("Knox key file err: %s", err.Error())
	}
	defer f.Close()
	err = json.NewDecoder(f).Decode(&key)
	if err != nil {
		return fmt.Errorf("Knox json decode err: %s", err.Error())
	}
	c.setValues(&key)
	return nil
}

func (c *fileClient) setValues(key *Key) {
	c.Lock()
	defer c.Unlock()
	c.keyObject = *key
	c.primary = string(key.VersionList.GetPrimary().Data)
	ks := key.VersionList.GetActive()
	c.active = make([]string, len(ks))
	for _, kv := range ks {
		c.active = append(c.active, string(kv.Data))
	}
}

func (c *fileClient) GetPrimary() string {
	c.RLock()
	defer c.RUnlock()
	return c.primary
}

func (c *fileClient) GetActive() []string {
	c.RLock()
	defer c.RUnlock()
	return c.active
}

func (c *fileClient) GetKeyObject() Key {
	c.RLock()
	defer c.RUnlock()
	return c.keyObject
}

// NewFileClient creates a file watcher knox client for the keyID given (it refreshes every ten seconds).
// This client calls `knox register` to cache the key locally on the file system.
func NewFileClient(keyID string) (Client, error) {
	var key Key
	c := &fileClient{keyID: keyID}
	jsonKey, err := Register(keyID)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(jsonKey, &key)
	if err != nil {
		return nil, fmt.Errorf("Knox json decode err: %s", err.Error())
	}
	c.setValues(&key)
	go func() {
		for range time.Tick(refresh) {
			err := c.update()
			if err != nil {
				log.Println("Failed to update knox key ", err.Error())
			}
		}
	}()
	return c, nil
}

// NewMockKeyVersion creates a Knox KeyVersion to be used for testing
func NewMockKeyVersion(keydata []byte, status VersionStatus) KeyVersion {
	return KeyVersion{Data: keydata, Status: status}
}

// NewMock is a knox Client to be used for testing.
func NewMock(primary string, active []string) Client {
	var kvl []KeyVersion
	kvl = append(kvl, NewMockKeyVersion([]byte(primary), Primary))
	for _, data := range active {
		kvl = append(kvl, NewMockKeyVersion([]byte(data), Active))
	}

	return &fileClient{primary: primary, active: active, keyObject: Key{VersionList: KeyVersionList(kvl)}}
}

// Register registers the given keyName with knox. If the operation fails, it returns an error.
func Register(keyID string) ([]byte, error) {
	cmd := exec.Command("knox", "register", "-g", "-k", keyID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("error getting knox key: %s %v '%q'", keyID, err, output)
	}
	return output, nil
}

// GetBackoffDuration returns a time duration to sleep based on the attempt #.
func GetBackoffDuration(attempt int) time.Duration {
	basef := float64(baseBackoff)
	// Add some randomness.
	duration := rand.Float64()*float64(attempt) + basef

	if duration > float64(maxBackoff) {
		return maxBackoff
	}
	return time.Duration(duration)
}

// APIClient is an interface that talks to the knox server for key management.
type APIClient interface {
	GetKey(keyID string) (*Key, error)
	CreateKey(keyID string, data []byte, acl ACL) (uint64, error)
	GetKeys(keys map[string]string) ([]string, error)
	DeleteKey(keyID string) error
	GetACL(keyID string) (*ACL, error)
	PutAccess(keyID string, acl ...Access) error
	AddVersion(keyID string, data []byte) (uint64, error)
	UpdateVersion(keyID, versionID string, status VersionStatus) error
	CacheGetKey(keyID string) (*Key, error)
	NetworkGetKey(keyID string) (*Key, error)
	GetKeyWithStatus(keyID string, status VersionStatus) (*Key, error)
	CacheGetKeyWithStatus(keyID string, status VersionStatus) (*Key, error)
	NetworkGetKeyWithStatus(keyID string, status VersionStatus) (*Key, error)
}

type HTTP interface {
	Do(req *http.Request) (*http.Response, error)
}

// HTTPClient is a client that uses HTTP to talk to Knox.
type HTTPClient struct {
	// Host is used as the host for http connections
	Host string
	//AuthHandler returns the authorization string for authenticating to knox. Users should be prefixed by 0u, machines by 0m. On fail, return empty string.
	AuthHandler func() string
	// KeyFolder is the location of cached keys on the file system. If empty, does not check for cached keys.
	KeyFolder string
	// Client is the http client for making network calls
	Client HTTP
	// Version is the current client version, useful for debugging and sent as a header
	Version string
}

// NewClient creates a new client to connect to talk to Knox.
func NewClient(host string, client HTTP, authHandler func() string, keyFolder, version string) APIClient {
	return &HTTPClient{
		Host:        host,
		Client:      client,
		AuthHandler: authHandler,
		KeyFolder:   keyFolder,
		Version:     version,
	}
}

// CacheGetKey gets the key from file system cache.
func (c *HTTPClient) CacheGetKey(keyID string) (*Key, error) {
	if c.KeyFolder == "" {
		return nil, fmt.Errorf("No folder set for cached key.")
	}
	path := c.KeyFolder + keyID
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k := Key{Path: path}
	err = json.Unmarshal(b, &k)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// NetworkGetKey gets a knox key by keyID and only uses network without the caches.
func (c *HTTPClient) NetworkGetKey(keyID string) (*Key, error) {
	key := &Key{}
	err := c.getHTTPData("GET", "/v0/keys/"+keyID+"/", nil, key)
	return key, err
}

// GetKey gets a knox key by keyID.
func (c *HTTPClient) GetKey(keyID string) (*Key, error) {
	key, err := c.CacheGetKey(keyID)
	if err != nil {
		return c.NetworkGetKey(keyID)
	}
	return key, err
}

// CacheGetKeyWithStatus gets the key with status from file system cache.
func (c *HTTPClient) CacheGetKeyWithStatus(keyID string, status VersionStatus) (*Key, error) {
	if c.KeyFolder == "" {
		return nil, fmt.Errorf("No folder set for cached key.")
	}
	st, err := status.MarshalJSON()
	if err != nil {
		return nil, err
	}
	path := c.KeyFolder + keyID + "?status=" + string(st)
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k := Key{Path: path}
	err = json.Unmarshal(b, &k)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// NetworkGetKeyWithStatus gets a knox key by keyID and given version status (always calls network).
func (c *HTTPClient) NetworkGetKeyWithStatus(keyID string, status VersionStatus) (*Key, error) {
	// If clients need to know
	s, err := status.MarshalJSON()
	if err != nil {
		return nil, err
	}

	key := &Key{}
	err = c.getHTTPData("GET", "/v0/keys/"+keyID+"/?status="+string(s), nil, key)
	return key, err
}

// GetKeyWithStatus gets a knox key by keyID and status (leverages cache).
func (c *HTTPClient) GetKeyWithStatus(keyID string, status VersionStatus) (*Key, error) {
	key, err := c.CacheGetKeyWithStatus(keyID, status)
	if err != nil {
		return c.NetworkGetKeyWithStatus(keyID, status)
	}
	return key, err
}

// CreateKey creates a knox key with given keyID data and ACL.
func (c *HTTPClient) CreateKey(keyID string, data []byte, acl ACL) (uint64, error) {
	var i uint64
	d := url.Values{}
	d.Set("id", keyID)
	d.Set("data", base64.StdEncoding.EncodeToString(data))
	s, err := json.Marshal(acl)
	if err != nil {
		return i, err
	}
	d.Set("acl", string(s))
	err = c.getHTTPData("POST", "/v0/keys/", d, &i)
	return i, err
}

// GetKeys gets all Knox (if empty map) or gets all keys in map that do not match key version hash.
func (c *HTTPClient) GetKeys(keys map[string]string) ([]string, error) {
	var l []string

	d := url.Values{}
	for k, v := range keys {
		d.Set(k, v)
	}
	err := c.getHTTPData("GET", "/v0/keys/?"+d.Encode(), nil, &l)
	return l, err
}

// DeleteKey deletes a key from Knox.
func (c HTTPClient) DeleteKey(keyID string) error {
	err := c.getHTTPData("DELETE", "/v0/keys/"+keyID+"/", nil, nil)
	return err
}

// GetACL gets a knox key by keyID.
func (c *HTTPClient) GetACL(keyID string) (*ACL, error) {
	acl := &ACL{}
	err := c.getHTTPData("GET", "/v0/keys/"+keyID+"/access/", nil, acl)
	return acl, err
}

// PutAccess will add an ACL rule to a specific key.
func (c *HTTPClient) PutAccess(keyID string, a ...Access) error {
	d := url.Values{}
	s, err := json.Marshal(a)
	if err != nil {
		return err
	}
	d.Set("acl", string(s))
	err = c.getHTTPData("PUT", "/v0/keys/"+keyID+"/access/", d, nil)
	return err
}

// AddVersion adds a key version to a specific key.
func (c *HTTPClient) AddVersion(keyID string, data []byte) (uint64, error) {
	var i uint64
	d := url.Values{}
	d.Set("data", base64.StdEncoding.EncodeToString(data))
	err := c.getHTTPData("POST", "/v0/keys/"+keyID+"/versions/", d, &i)
	return i, err
}

// UpdateVersion either promotes or demotes a specific key version.
func (c *HTTPClient) UpdateVersion(keyID, versionID string, status VersionStatus) error {
	d := url.Values{}
	s, err := status.MarshalJSON()
	if err != nil {
		return err
	}
	d.Set("status", string(s))

	err = c.getHTTPData("PUT", "/v0/keys/"+keyID+"/versions/"+versionID+"/", d, nil)
	return err
}

func (c *HTTPClient) getClient() (HTTP, error) {
	if c.Client == nil {
		c.Client = &http.Client{}
	}
	return c.Client, nil
}

func (c *HTTPClient) getHTTPData(method string, path string, body url.Values, data interface{}) error {
	r, err := http.NewRequest(method, "https://"+c.Host+path, bytes.NewBufferString(body.Encode()))

	if err != nil {
		return err
	}

	auth := c.AuthHandler()
	switch {
	case auth == "":
		return fmt.Errorf("no authentication data given. Use 'knox login' or set KNOX_USER_AUTH or KNOX_MACHINE_AUTH")
	case auth[:2] == "0u":
		r.Header.Set("Authorization", auth[2:])
	case auth[:2] == "0s":
	default:
		return fmt.Errorf("wrong authentication type. Use 'knox login' or set KNOX_USER_AUTH or KNOX_MACHINE_AUTH")
	}
	r.Header.Set("User-Agent", fmt.Sprintf("Knox_Client/%s", c.Version))

	if body != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	cli, err := c.getClient()
	if err != nil {
		return err
	}

	resp := &Response{}
	resp.Data = data
	// Contains retry logic if we decode a 500 error.
	for i := 1; i <= maxRetryAttempts; i++ {
		err = getHTTPResp(cli, r, resp)
		if err != nil {
			return fmt.Errorf("bad response from server %+v\nerror: %v", resp, err)
		}
		if resp.Status != "ok" {
			if (resp.Code != InternalServerErrorCode) || (i == maxRetryAttempts) {
				return fmt.Errorf(resp.Message)
			}
			time.Sleep(GetBackoffDuration(i))
		} else {
			break
		}
	}

	return nil
}

func getHTTPResp(cli HTTP, r *http.Request, resp *Response) error {
	w, err := cli.Do(r)
	if err != nil {
		return err
	}
	defer w.Body.Close()

	decoder := json.NewDecoder(w.Body)
	return decoder.Decode(resp)
}

// MockClient builds a client that ignores certs and talks to the given host.
func MockClient(host string) *HTTPClient {
	return &HTTPClient{
		Host: host,
		AuthHandler: func() string {
			return "0uTESTAUTH"
		},
		KeyFolder: "",
		Client:    &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
		Version:   "mock",
	}
}
