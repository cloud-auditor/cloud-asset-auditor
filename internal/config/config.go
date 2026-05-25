// Package config wires viper with the project's standard precedence:
// flag > env (AUDITOR_*) > ./auditor.yaml > $HOME/.config/auditor.yaml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// EnvPrefix is the prefix for environment-variable overrides.
// e.g. `AUDITOR_OUTPUT_FORMAT=csv` overrides the audit command's -o default.
const EnvPrefix = "AUDITOR"

// Init builds a configured viper instance. If configFile is non-empty it
// pins the loader to that file; otherwise the standard search paths apply.
// A missing config file is not an error — flags and env can still drive the
// run on their own.
func Init(configFile string) (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv()

	if configFile != "" {
		v.SetConfigFile(configFile)
	} else {
		v.SetConfigName("auditor")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		if home, err := os.UserHomeDir(); err == nil {
			v.AddConfigPath(filepath.Join(home, ".config"))
		}
	}

	if err := v.ReadInConfig(); err != nil {
		var nf viper.ConfigFileNotFoundError
		if !errors.As(err, &nf) {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}
	return v, nil
}
