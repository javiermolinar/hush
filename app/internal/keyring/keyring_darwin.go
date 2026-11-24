package keyring

import (
	"context"
	"unsafe"

	"github.com/ebitengine/purego"
)

const itemNotFound int32 = -25300

type keychainAPI struct {
	noInteraction func(uint8) int32
	defaultChain  func(*uintptr) int32
	status        func(uintptr, *uint32) int32
	find          func(uintptr, uint32, string, uint32, string, *uint32, *unsafe.Pointer, *uintptr) int32
	add           func(uintptr, uint32, string, uint32, string, uint32, *byte, *uintptr) int32
	update        func(uintptr, uintptr, uint32, *byte) int32
	remove        func(uintptr) int32
	freeContent   func(uintptr, unsafe.Pointer) int32
	release       func(uintptr)
}

func loadKeychain() (keychainAPI, error) {
	var api keychainAPI
	security, err := purego.Dlopen("/System/Library/Frameworks/Security.framework/Security", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return api, errUnavailable
	}
	cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return api, errUnavailable
	}
	// The helper is short-lived. Keep both frameworks loaded until exit.
	purego.RegisterLibFunc(&api.noInteraction, security, "SecKeychainSetUserInteractionAllowed")
	purego.RegisterLibFunc(&api.defaultChain, security, "SecKeychainCopyDefault")
	purego.RegisterLibFunc(&api.status, security, "SecKeychainGetStatus")
	purego.RegisterLibFunc(&api.find, security, "SecKeychainFindGenericPassword")
	purego.RegisterLibFunc(&api.add, security, "SecKeychainAddGenericPassword")
	purego.RegisterLibFunc(&api.update, security, "SecKeychainItemModifyAttributesAndData")
	purego.RegisterLibFunc(&api.remove, security, "SecKeychainItemDelete")
	purego.RegisterLibFunc(&api.freeContent, security, "SecKeychainItemFreeContent")
	purego.RegisterLibFunc(&api.release, cf, "CFRelease")
	return api, nil
}

func platformKeyring(_ context.Context, req request) ([]byte, error) {
	api, err := loadKeychain()
	if err != nil {
		return nil, err
	}
	// Process-local and set before the first keychain access: no unlock or
	// item-access dialog is allowed, even when invoked from the desktop.
	if api.noInteraction(0) != 0 {
		return nil, errUnavailable
	}
	var chain uintptr
	if api.defaultChain(&chain) != 0 || chain == 0 {
		return nil, errUnavailable
	}
	defer api.release(chain)
	return api.perform(chain, req)
}

func (a keychainAPI) perform(chain uintptr, req request) ([]byte, error) {
	var status uint32
	const unlocked = 1 // kSecUnlockStateStatus
	if a.status(chain, &status) != 0 || status&unlocked == 0 {
		return nil, errUnavailable
	}
	service := servicePrefix + req.Scope
	var item uintptr
	var data unsafe.Pointer
	var size uint32
	var length *uint32
	var stored *unsafe.Pointer
	if req.Operation == "get" {
		length, stored = &size, &data
	}
	rc := a.find(chain, uint32(len(service)), service, uint32(len(account)), account, length, stored, &item)
	if item != 0 {
		defer a.release(item)
	}
	if data != nil {
		defer a.freeContent(0, data)
	}
	if rc == itemNotFound {
		if req.Operation != "set" {
			return nil, nil
		}
		if a.add(chain, uint32(len(service)), service, uint32(len(account)), account,
			uint32(len(req.Record)), &req.Record[0], nil) == 0 {
			return nil, nil
		}
		return nil, errUnavailable
	}
	if rc != 0 || item == 0 {
		return nil, errUnavailable
	}
	switch req.Operation {
	case "get":
		if data == nil || size == 0 || size > maxCredential {
			return nil, errUnavailable
		}
		return append([]byte(nil), unsafe.Slice((*byte)(data), int(size))...), nil
	case "set":
		rc = a.update(item, 0, uint32(len(req.Record)), &req.Record[0])
	case "delete":
		rc = a.remove(item)
	}
	if rc != 0 {
		return nil, errUnavailable
	}
	return nil, nil
}
