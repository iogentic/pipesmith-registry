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
	LogDir      string // log_dir of config.json, empty for the default; serve takes its option and variable first (LogDirFrom)
	Stores      []StoreConfig
	DockerPorts map[string]int // repository -> port; wins over the definition
	// Writes is the key writes of config.json, and only that: serve takes
	// its option and the environment first (WritesFrom), so that a command
	// that saves the configuration never stores either of them.
	Writes bool
	// Upstream is the key upstream of config.json, and only that, as Writes
	// is (UpstreamFrom; decision 5.1 of the phase 5 plan).
	Upstream bool
	// TLSCert and TLSKey are tls_cert and tls_key: the PEM files of the TLS
	// listeners; "" when absent.
	TLSCert string
	TLSKey  string
	// TrustedProxies is trusted_proxies as written: the addresses and
	// networks of the reverse proxies whose forwarded headers count (see
	// ParseTrustedProxies); nil when absent or empty.
	TrustedProxies []string
	// ExportMaxFiles and ExportMaxBytes are export_max_files and
	// export_max_bytes (call B18 of docs/plans/browse-tools.md): the bounds of
	// one export of a folder; 0 when absent or at the default of 20,000 files
	// and 2 GiB, which the server takes for 0.
	ExportMaxFiles, ExportMaxBytes int64
}
