package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/pavelzhurov/knox"
	"github.com/pavelzhurov/knox/server/auth"
)

var routes = [...]Route{
	{
		Method:  "GET",
		Id:      "getkeys",
		Path:    "/v0/keys/",
		Handler: getKeysHandler,
		Parameters: []Parameter{
			RawQueryParameter("queryString"),
		},
	},
	{
		Method:  "POST",
		Id:      "postkeys",
		Path:    "/v0/keys/",
		Handler: postKeysHandler,
		Parameters: []Parameter{
			PostParameter("id"),
			PostParameter("data"),
			PostParameter("acl"),
		},
	},

	{
		Method:  "GET",
		Id:      "getkey",
		Path:    "/v0/keys/{keyID}/",
		Handler: getKeyHandler,
		Parameters: []Parameter{
			UrlParameter("keyID"),
			QueryParameter("status"),
		},
	},
	{
		Method:  "DELETE",
		Id:      "deletekey",
		Path:    "/v0/keys/{keyID}/",
		Handler: deleteKeyHandler,
		Parameters: []Parameter{
			UrlParameter("keyID"),
		},
	},
	{
		Method:  "GET",
		Id:      "getaccess",
		Path:    "/v0/keys/{keyID}/access/",
		Handler: getAccessHandler,
		Parameters: []Parameter{
			UrlParameter("keyID"),
		},
	},
	{
		Method:  "PUT",
		Id:      "putaccess",
		Path:    "/v0/keys/{keyID}/access/",
		Handler: putAccessHandler,
		Parameters: []Parameter{
			UrlParameter("keyID"),
			PostParameter("access"),
			PostParameter("acl"),
		},
	},
	{
		Method:  "POST",
		Id:      "postversion",
		Path:    "/v0/keys/{keyID}/versions/",
		Handler: postVersionHandler,
		Parameters: []Parameter{
			UrlParameter("keyID"),
			PostParameter("data"),
		},
	},
	{
		Method:  "PUT",
		Id:      "putversion",
		Path:    "/v0/keys/{keyID}/versions/{versionID}/",
		Handler: putVersionsHandler,
		Parameters: []Parameter{
			UrlParameter("keyID"),
			UrlParameter("versionID"),
			PostParameter("status"),
		},
	},
}

// getKeysHandler is a handler that gets key IDs specified in the request.
//
// This returns all keys if no keyIds are passed in. Otherwise it returns the requested Key IDs that have been changed.
// It is used for both discovering what keys are available and for finding which keys have updates available. Keys are passed in as url parameters.
// This is going to have url length problems when a large number of keys are
// requested. A proposed fix is to just use the request body but that violates
// REST so that fix will be postponed until this actually is a problem.
// The route for this handler is GET /v0/keys/
// There are no authorization constraints on this route. UPDATE: Now there are :)
func getKeysHandler(m KeyManager, principal knox.Principal, parameters map[string]string) (interface{}, *HTTPError) {

	queryString := parameters["queryString"]

	if !principal.CanAccessOPA(m.GetAuthenticator(), "", "ListKeys", "pvc", "kms") {
		return nil, errF(knox.UnauthorizedCode, fmt.Sprintf("Principal %s not authorized to list keys", principal.GetID()))
	}

	// Can't throw error since direct from a http request
	keyMap, _ := url.ParseQuery(queryString)
	keyM := map[string]string{}
	for k := range keyMap {
		for _, v := range keyMap[k] {
			keyM[k] = v
		}
	}

	// Get necessary data based on parameters
	if len(keyMap) == 0 {
		keys, err := m.GetAllKeyIDs()
		if err != nil {
			return nil, errF(knox.InternalServerErrorCode, err.Error())
		}
		verified_keys, err := verifyKeys(m, principal, keys)
		if err != nil {
			return nil, errF(knox.InternalServerErrorCode, err.Error())
		}
		return verified_keys, nil
	}

	keys, err := m.GetUpdatedKeyIDs(keyM)
	if err != nil {
		return nil, errF(knox.InternalServerErrorCode, err.Error())
	}
	verified_keys, err := verifyKeys(m, principal, keys)
	if err != nil {
		return nil, errF(knox.InternalServerErrorCode, err.Error())
	}
	return verified_keys, nil
}

func CanAccess(principal knox.Principal, m KeyManager, acl knox.ACL, at knox.AccessType, keyID, action, partition, service string) bool {
	switch m.GetAuthorizationType() {
	case AclAuthorization:
		return principal.CanAccess(acl, at)
	case OpaAuthorization:
		return principal.CanAccessOPA(m.GetAuthenticator(), keyID, action, partition, service)
	}
	return false
}

// Authorize access to keys. If user is not authorized to read key, it won't be returned
func verifyKeys(m KeyManager, principal knox.Principal, keys []string) ([]string, error) {
	var return_keys []string

	for _, keyID := range keys {
		key, err := m.GetKey(keyID, knox.Active)
		if err != nil {
			return nil, fmt.Errorf("can't verify principal %s access to one of the keys", principal.GetID())
		}

		if CanAccess(principal, m, key.ACL, knox.Read, keyID, "GetKey", "pvc", "kms") {
			return_keys = append(return_keys, keyID)
		}
	}
	return return_keys, nil
}

// postKeysHandler creates a new key and stores it. It reads from the post data
// key ID, base64 encoded data, and JSON encoded ACL.
// It returns the key version ID of the original Primary key version.
// The route for this handler is POST /v0/keys/
// The postKeysHandler must be a User.
func postKeysHandler(m KeyManager, principal knox.Principal, parameters map[string]string) (interface{}, *HTTPError) {

	keyID, keyIDOK := parameters["id"]
	if !keyIDOK {
		return nil, errF(knox.NoKeyIDCode, "Missing parameter 'id'")
	}

	// Authorize
	if (!auth.IsUser(principal) && m.GetAuthorizationType() == AclAuthorization) ||
		(!principal.CanAccessOPA(m.GetAuthenticator(), keyID, "CreateKey", "pvc", "kms") && m.GetAuthorizationType() == OpaAuthorization) {
		return nil, errF(knox.UnauthorizedCode, fmt.Sprintf("Principal %s not authorized to create key", principal.GetID()))
	}

	data, dataOK := parameters["data"]
	if !dataOK {
		return nil, errF(knox.NoKeyDataCode, "Missing parameter 'data'")
	}
	aclStr, aclOK := parameters["acl"]

	acl := make(knox.ACL, 0)
	if aclOK {
		jsonErr := json.Unmarshal([]byte(aclStr), &acl)
		if jsonErr != nil {
			return nil, errF(knox.BadRequestDataCode, jsonErr.Error())
		}
	}

	decodedData, decodeErr := base64.StdEncoding.DecodeString(data)
	if decodeErr != nil {
		return nil, errF(knox.BadRequestDataCode, decodeErr.Error())
	}

	// Create and add new key
	key := newKey(keyID, acl, decodedData, principal)
	err := m.AddNewKey(&key)
	if err != nil {
		if err == knox.ErrKeyExists {
			return nil, errF(knox.KeyIdentifierExistsCode, fmt.Sprintf("Key %s already exists", keyID))
		}
		if err == knox.ErrInvalidKeyID {
			return nil, errF(knox.BadKeyFormatCode, fmt.Sprintf("KeyID includes unsupported characters %s", keyID))
		}

		return nil, errF(knox.InternalServerErrorCode, err.Error())
	}
	return key.VersionList[0].ID, nil
}

// getKeyHandler gets the key matching the keyID in the request.
// The route for this handler is GET /v0/keys/<key_id>/
// The principal must have Read access to the key
func getKeyHandler(m KeyManager, principal knox.Principal, parameters map[string]string) (interface{}, *HTTPError) {
	keyID := parameters["keyID"]

	status := knox.Active
	statusStr, statusOK := parameters["status"]
	if statusOK {
		statusErr := status.UnmarshalJSON([]byte(statusStr))
		if statusErr != nil {
			return nil, errF(knox.BadRequestDataCode, statusErr.Error())
		}
	}

	// Get data
	key, getErr := m.GetKey(keyID, status)
	if getErr != nil {
		if getErr == knox.ErrKeyIDNotFound {
			return nil, errF(knox.KeyIdentifierDoesNotExistCode, fmt.Sprintf("No such key %s", keyID))
		}
		return nil, errF(knox.InternalServerErrorCode, getErr.Error())
	}

	// Authorize access to data
	if !CanAccess(principal, m, key.ACL, knox.Read, keyID, "GetKey", "pvc", "kms") {
		return nil, errF(knox.UnauthorizedCode, fmt.Sprintf("Principal %s not authorized to read %s", principal.GetID(), keyID))
	}
	// Zero ACL for key response, in order to avoid caching unnecessarily
	key.ACL = knox.ACL{}
	return key, nil
}

// deleteKeyHandler deletes the key matching the keyID in the request.
// The route for this handler is DELETE /v0/keys/<key_id>/
// The principal needs Admin access to the key.
func deleteKeyHandler(m KeyManager, principal knox.Principal, parameters map[string]string) (interface{}, *HTTPError) {
	keyID := parameters["keyID"]

	key, getErr := m.GetKey(keyID, knox.Primary)
	if getErr != nil {
		if getErr == knox.ErrKeyIDNotFound {
			return nil, errF(knox.KeyIdentifierDoesNotExistCode, fmt.Sprintf("No such key %s", keyID))
		}
		return nil, errF(knox.InternalServerErrorCode, getErr.Error())
	}

	// Authorize
	if !CanAccess(principal, m, key.ACL, knox.Admin, keyID, "DeleteKey", "pvc", "kms") {
		return nil, errF(knox.UnauthorizedCode, fmt.Sprintf("Principal %s not authorized to delete %s", principal.GetID(), keyID))
	}

	// Delete the key
	err := m.DeleteKey(keyID)
	if err != nil {
		return nil, errF(knox.InternalServerErrorCode, err.Error())
	}
	return nil, nil
}

// getAccessHandler gets the ACL for a specific Key.
// The route for this handler is GET /v0/keys/<key_id>/access/
func getAccessHandler(m KeyManager, principal knox.Principal, parameters map[string]string) (interface{}, *HTTPError) {

	keyID := parameters["keyID"]

	// Get the key
	key, getErr := m.GetKey(keyID, knox.Primary)
	if getErr != nil {
		if getErr == knox.ErrKeyIDNotFound {
			return nil, errF(knox.KeyIdentifierDoesNotExistCode, fmt.Sprintf("No such key %s", keyID))
		}
		return nil, errF(knox.InternalServerErrorCode, getErr.Error())
	}

	if !CanAccess(principal, m, key.ACL, knox.Read, keyID, "GetACL", "pvc", "kms") {
		return nil, errF(knox.UnauthorizedCode, fmt.Sprintf("Principal %s not authorized to get acl %s", principal.GetID(), keyID))
	}

	return key.ACL, nil
}

// putAccessHandler adds or updates the existing ACL with an Access object
// This object is input as base64 encoded json encoded form data
// access is used for a single access rule and acl is used for multiple rules
// existing access rules will not be modified unless the same Type and Name is used
// The route for this handler is PUT /v0/keys/<key_id>/access/
// The principal needs Admin access.
func putAccessHandler(m KeyManager, principal knox.Principal, parameters map[string]string) (interface{}, *HTTPError) {
	keyID := parameters["keyID"]

	accessStr, accessOK := parameters["access"]
	aclStr, aclOK := parameters["acl"]

	acl := []knox.Access{}
	if accessOK {
		access := knox.Access{}
		// If JSON decode fails, try a base64 encoded JSON string (both options are around for backwards compatibility)
		jsonErr := json.Unmarshal([]byte(accessStr), &access)
		if jsonErr != nil {
			decodedData, decodeErr := base64.RawURLEncoding.DecodeString(accessStr)
			if decodeErr != nil {
				return nil, errF(knox.BadRequestDataCode, decodeErr.Error())
			}
			jsonErr := json.Unmarshal(decodedData, &access)
			if jsonErr != nil {
				return nil, errF(knox.BadRequestDataCode, jsonErr.Error())
			}
		}
		acl = append(acl, access)
	} else if aclOK {
		jsonErr := json.Unmarshal([]byte(aclStr), &acl)
		if jsonErr != nil {
			return nil, errF(knox.BadRequestDataCode, jsonErr.Error())
		}
	} else {
		return nil, errF(knox.BadRequestDataCode, "Missing acl and access parameters")
	}

	// Get the Key
	key, getErr := m.GetKey(keyID, knox.Primary)
	if getErr != nil {
		if getErr == knox.ErrKeyIDNotFound {
			return nil, errF(knox.KeyIdentifierDoesNotExistCode, fmt.Sprintf("No such key %s", keyID))
		}
		return nil, errF(knox.InternalServerErrorCode, getErr.Error())
	}

	// Authorize
	if !CanAccess(principal, m, key.ACL, knox.Admin, keyID, "PutACL", "pvc", "kms") {
		return nil, errF(knox.UnauthorizedCode, fmt.Sprintf("Principal %s not authorized to update access for %s", principal.GetID(), keyID))
	}

	for _, access := range acl {
		// If access type change is not "None" (i.e. we're adding, not deleting, an ACL entry) then
		// we apply validation on the ID string to make sure it conforms to the expectations of the
		// particular principal type. We do this to block empty machines prefixes and other invalid
		// or bad entries.
		if access.AccessType != knox.None {
			principalErr := access.Type.IsValidPrincipal(access.ID, extraPrincipalValidators)
			if principalErr != nil {
				return nil, errF(knox.BadPrincipalIdentifier, principalErr.Error())
			}
		}
	}

	// Update Access
	updateErr := m.UpdateAccess(keyID, acl...)
	if updateErr != nil {
		return nil, errF(knox.InternalServerErrorCode, updateErr.Error())
	}
	return nil, nil
}

// postVersionHandler creates a new key version. This version is immediately
// added as an Active key.
// The route for this handler is PUT /v0/keys/<key_id>/versions/
// The principal needs Write access.
func postVersionHandler(m KeyManager, principal knox.Principal, parameters map[string]string) (interface{}, *HTTPError) {

	keyID := parameters["keyID"]
	dataStr, dataOK := parameters["data"]
	if !dataOK {
		return nil, errF(knox.BadRequestDataCode, "Missing parameter 'data'")
	}
	decodedData, decodeErr := base64.StdEncoding.DecodeString(dataStr)
	if decodeErr != nil {
		return nil, errF(knox.BadRequestDataCode, decodeErr.Error())
	}

	// Get the key
	key, getErr := m.GetKey(keyID, knox.Inactive)
	if getErr != nil {
		if getErr == knox.ErrKeyIDNotFound {
			return nil, errF(knox.KeyIdentifierDoesNotExistCode, fmt.Sprintf("No such key %s", keyID))
		}
		return nil, errF(knox.InternalServerErrorCode, getErr.Error())
	}

	// Authorize
	if !CanAccess(principal, m, key.ACL, knox.Write, keyID, "CreateVersion", "pvc", "kms") {
		return nil, errF(knox.UnauthorizedCode, fmt.Sprintf("Principal %s not authorized to write %s", principal.GetID(), keyID))
	}

	// Create and add the new version
	version := newKeyVersion(decodedData, knox.Active)

	err := m.AddVersion(keyID, &version)

	if err != nil {
		return nil, errF(knox.InternalServerErrorCode, err.Error())
	}
	return version.ID, nil
}

// putVersionsHandler rotates key versions by changing the version status.
// It takes the new status as input. Accepted inputs include:
// If the key version is Inactive, it can become Active.
// If the key version is Active, it can become Inactive or Primary. Note that,
//   this will change the current Primary key to Active.
// If the key version is Primary, the version status cannot be changed. Instead
//   promote another key version to Primary to replace it.
// The route for this handler is PUT /v0/keys/<key_id>/versions/<version_id>/
// The principal needs Write access.
func putVersionsHandler(m KeyManager, principal knox.Principal, parameters map[string]string) (interface{}, *HTTPError) {

	keyID := parameters["keyID"]
	versionID := parameters["versionID"]

	statusStr, statusOK := parameters["status"]
	if !statusOK {
		return nil, errF(knox.BadRequestDataCode, "Missing parameter 'status'")
	}
	status := knox.Active
	statusErr := status.UnmarshalJSON([]byte(statusStr))
	if statusErr != nil {
		return nil, errF(knox.BadRequestDataCode, statusErr.Error())
	}
	id, intErr := strconv.ParseUint(versionID, 10, 64)
	if intErr != nil {
		return nil, errF(knox.BadRequestDataCode, intErr.Error())
	}

	// Get the key
	key, getErr := m.GetKey(keyID, knox.Inactive)
	if getErr != nil {
		if getErr == knox.ErrKeyIDNotFound {
			return nil, errF(knox.KeyIdentifierDoesNotExistCode, fmt.Sprintf("No such key %s", keyID))
		}
		return nil, errF(knox.InternalServerErrorCode, getErr.Error())
	}

	// Authorize
	if !CanAccess(principal, m, key.ACL, knox.Write, keyID, "PutVersion", "pvc", "kms") {
		return nil, errF(knox.UnauthorizedCode, fmt.Sprintf("Principal %s not authorized to write %s", principal.GetID(), keyID))
	}

	err := m.UpdateVersion(keyID, id, status)

	switch err {
	case nil:
		return nil, nil
	case knox.ErrKeyVersionNotFound:
		return nil, errF(knox.KeyVersionDoesNotExistCode, err.Error())
	case knox.ErrPrimaryToInactive, knox.ErrPrimaryToActive, knox.ErrInactiveToPrimary:
		return nil, errF(knox.BadRequestDataCode, err.Error())
	default:
		return nil, errF(knox.InternalServerErrorCode, err.Error())
	}
}
