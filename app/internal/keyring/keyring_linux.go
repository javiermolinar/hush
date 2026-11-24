package keyring

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"
	"github.com/javiermolinar/hush/app/internal/authproviders"
)

const secretService = "org.freedesktop.secrets"
const secretAPI = "org.freedesktop.Secret."
const servicePath = "/org/freedesktop/secrets"

// The Secret Service plain session sends the secret over the user's local,
// authenticated Unix D-Bus connection. Never dial a remote bus or auto-start
// a login/keyring service from a headless process.
type serviceSecret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

func platformKeyring(ctx context.Context, req request) ([]byte, error) {
	address := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if !strings.HasPrefix(address, "unix:") || strings.Contains(address, ";") {
		return nil, errUnavailable
	}
	bus, err := dbus.Connect(address, dbus.WithContext(ctx), dbus.WithAuth(dbus.AuthExternal(strconv.Itoa(os.Getuid()))))
	if err != nil {
		return nil, errUnavailable
	}
	defer bus.Close()
	call := func(path dbus.ObjectPath, method string, args ...any) *dbus.Call {
		return bus.Object(secretService, path).CallWithContext(ctx, method, dbus.FlagNoAutoStart, args...)
	}
	attributes := map[string]string{
		"application": "hush", "service": servicePrefix + req.Scope, "account": req.Provider,
	}
	var unlocked, locked []dbus.ObjectPath
	if call(servicePath, secretAPI+"Service.SearchItems", attributes).Store(&unlocked, &locked) != nil ||
		len(locked) != 0 || len(unlocked) > 1 {
		return nil, errUnavailable
	}
	var item, collection dbus.ObjectPath
	if len(unlocked) == 1 {
		item = unlocked[0]
		if !item.IsValid() || item == "/" {
			return nil, errUnavailable
		}
	} else {
		// A missing item in an unlocked default collection is not the same
		// as an inaccessible store. Never call Unlock, CreateCollection or Prompt.
		if call(servicePath, secretAPI+"Service.ReadAlias", "default").Store(&collection) != nil ||
			!collection.IsValid() || collection == "/" {
			return nil, errUnavailable
		}
		var state dbus.Variant
		if call(collection, "org.freedesktop.DBus.Properties.Get", secretAPI+"Collection", "Locked").Store(&state) != nil ||
			state.Value() != false {
			return nil, errUnavailable
		}
		if req.Operation != "set" {
			return nil, nil
		}
	}
	if req.Operation == "delete" {
		var prompt dbus.ObjectPath
		if call(item, secretAPI+"Item.Delete").Store(&prompt) != nil || prompt != "/" {
			return nil, errUnavailable
		}
		return nil, nil
	}

	var ignored dbus.Variant
	var session dbus.ObjectPath
	if call(servicePath, secretAPI+"Service.OpenSession", "plain", dbus.MakeVariant("")).Store(&ignored, &session) != nil ||
		!session.IsValid() || session == "/" {
		return nil, errUnavailable
	}
	if req.Operation == "get" {
		var secret serviceSecret
		if call(item, secretAPI+"Item.GetSecret", session).Store(&secret) != nil || secret.Session != session ||
			len(secret.Parameters) != 0 || len(secret.Value) == 0 || len(secret.Value) > maxCredential {
			return nil, errUnavailable
		}
		return secret.Value, nil
	}
	secret := serviceSecret{Session: session, Parameters: []byte{}, Value: req.Record, ContentType: "application/json"}
	if item != "" {
		if call(item, secretAPI+"Item.SetSecret", secret).Err != nil {
			return nil, errUnavailable
		}
	} else {
		provider, _ := authproviders.Lookup(req.Provider)
		props := map[string]dbus.Variant{
			secretAPI + "Item.Label":      dbus.MakeVariant("Hush " + provider.Label + " OAuth"),
			secretAPI + "Item.Attributes": dbus.MakeVariant(attributes),
		}
		var created, prompt dbus.ObjectPath
		if call(collection, secretAPI+"Collection.CreateItem", props, secret, false).Store(&created, &prompt) != nil ||
			!created.IsValid() || created == "/" || prompt != "/" {
			return nil, errUnavailable
		}
	}
	return nil, nil
}
