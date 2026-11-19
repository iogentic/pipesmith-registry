package manifest

import "os"

// zstdVariables are the only variables of the environment the zstd program
// gets; the bind passwords of the LDAP servers stay in this process.
var zstdVariables = []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"}

func zstdEnvironment() []string {
	env := []string{}
	for _, name := range zstdVariables {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// zstdRelativeError refuses a zstd that PATH finds only through a relative
// or empty entry, which names the working directory.
func zstdRelativeError(path string) error {
	return newError("OSError", "the zstd program to read %s is found only through a relative or empty PATH entry; "+
		"zstd runs only from an absolute PATH entry", path)
}
