// Package config holds the data directory, the bind address, the port, the
// attached stores and the Docker ports, stored in <data dir>/config.json.
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
}
