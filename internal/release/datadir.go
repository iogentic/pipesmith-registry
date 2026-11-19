package release

import (
	"fmt"
	"os"
)

// RequireDataDir is data_dir of update.json, which has to name an existing
// directory. update and rollback take the data directory of the unit from
// there and never from the environment of the caller.
func RequireDataDir(cfg *UpdateConfig) (string, error) {
	if cfg.DataDir == "" {
		return "", fmt.Errorf("%w: %s has no data_dir; set it to the data directory of the unit", ErrConfig, cfg.Path)
	}
	info, err := os.Stat(cfg.DataDir)
	if err != nil {
		return "", fmt.Errorf("%w: data_dir of %s: %v", ErrConfig, cfg.Path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: data_dir of %s: %s is not a directory", ErrConfig, cfg.Path, cfg.DataDir)
	}
	return cfg.DataDir, nil
}

// writeCheck creates and removes a file in the data directory when the plan
// holds a migration, because the way back writes there.
func (u *Updater) writeCheck(p *schemaPlan) error {
	if len(p.files) == 0 {
		return nil
	}
	err := u.at("write-check")
	if err == nil {
		var f *os.File
		if f, err = os.CreateTemp(p.dataDir, ".pipesmith-registry-write-check-"); err == nil {
			err = f.Close()
			if rerr := os.Remove(f.Name()); err == nil {
				err = rerr
			}
		}
	}
	if err != nil {
		return fmt.Errorf("%w: cannot write %s: %v; the way back of a migration writes there", ErrSpace, p.dataDir, err)
	}
	return nil
}
