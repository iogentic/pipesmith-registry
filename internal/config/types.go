// Package config holds the data directory, the bind address, the port, the
// attached stores, the Docker ports and the process values of process.go,
// stored in <data dir>/config.json.
package config

// StoreConfig is one attached store.
type StoreConfig struct {
	Name     string
	Kind     string // "nexus-file" or "directory"
	Root     string
	Repos    []string // directory stores: limit to these top level names; nil means none given
	Manifest *string  // directory stores only: a blob manifest file or directory
}

// Config is the configuration of one data directory.
type Config struct {
	DataDir     string
	Bind        string
	Port        int
	Stores      []StoreConfig
	DockerPorts map[string]int // repository -> port; wins over the definition
	// Writes is the key writes of config.json, and only that: serve takes
	// its option and the environment first (WritesFrom), so that a command
	// that saves the configuration never stores either of them.
	Writes bool
	// TLSCert and TLSKey are tls_cert and tls_key: the PEM files of the TLS
	// listeners; "" when absent.
	TLSCert string
	TLSKey  string
	// TrustedProxies is trusted_proxies as written: the addresses and
	// networks of the reverse proxies whose forwarded headers count (see
	// ParseTrustedProxies); nil when absent or empty.
	TrustedProxies []string
}
