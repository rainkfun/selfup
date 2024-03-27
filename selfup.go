// Package selfup implements daemonizable
// self-upgrading binaries in Go (golang).
package selfup

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/rainkfun/selfup/fetcher"
)

const (
	envSlaveID        = "OVERSEER_SLAVE_ID"
	envIsSlave        = "OVERSEER_IS_SLAVE"
	envNumFDs         = "OVERSEER_NUM_FDS"
	envBinID          = "OVERSEER_BIN_ID"
	envBinPath        = "OVERSEER_BIN_PATH"
	envBinCheck       = "OVERSEER_BIN_CHECK"
	envBinCheckLegacy = "GO_UPGRADE_BIN_CHECK"
)

// Config defines selfup's run-time configuration
type Config struct {
	//Required will prevent selfup from fallback to running
	//running the program in the main process on failure.
	Required bool
	//Program's main function
	Program func(state *State)
	//Program's zero-downtime socket listening address (set this or Addresses)
	Address string
	//Program's zero-downtime socket listening addresses (set this or Address)
	Addresses []string
	//RestartSignal will manually trigger a graceful restart. Defaults to SIGUSR2.
	RestartSignal os.Signal
	//TerminateTimeout controls how long selfup should
	//wait for the program to terminate itself. After this
	//timeout, selfup will issue a SIGKILL.
	TerminateTimeout time.Duration
	//MinFetchInterval defines the smallest duration between Fetch()s.
	//This helps to prevent unwieldy fetch.Interfaces from hogging
	//too many resources. Defaults to 1 second.
	MinFetchInterval time.Duration
	//PreUpgrade runs after a binary has been retrieved, user defined checks
	//can be run here and returning an error will cancel the upgrade.
	PreUpgrade func(tempBinaryPath string) error
	//NoRestart disables all restarts, this option essentially converts
	//the RestartSignal into a "ShutdownSignal".
	NoRestart bool
	//NoRestartAfterFetch disables automatic restarts after each upgrade.
	//Though manual restarts using the RestartSignal can still be performed.
	NoRestartAfterFetch bool
	//Fetcher will be used to fetch binaries.
	Fetcher fetcher.Interface
}

func validate(c *Config) error {
	//validate
	if c.Program == nil {
		return errors.New("selfup.Config.Program required")
	}
	if c.Address != "" {
		if len(c.Addresses) > 0 {
			return errors.New("selfup.Config.Address and Addresses cant both be set")
		}
		c.Addresses = []string{c.Address}
	} else if len(c.Addresses) > 0 {
		c.Address = c.Addresses[0]
	}
	if c.RestartSignal == nil {
		c.RestartSignal = SIGUSR2
	}
	if c.TerminateTimeout <= 0 {
		c.TerminateTimeout = 30 * time.Second
	}
	if c.MinFetchInterval <= 0 {
		c.MinFetchInterval = 1 * time.Second
	}
	return nil
}

// RunErr allows manual handling of any
// selfup errors.
func RunErr(c Config) error {
	return runErr(&c)
}

// Run executes selfup, if an error is
// encountered, selfup fallsback to running
// the program directly (unless Required is set).
func Run(c Config) {
	err := runErr(&c)
	if err != nil {
		if c.Required {
			mslog.Error("selfup disabled, run failed", "err", err)
			os.Exit(1)
		}
		mslog.Error("selfup disabled, run failed", "err", err)
		c.Program(&DisabledState)
		return
	}
	os.Exit(0)
}

// sanityCheck returns true if a check was performed
func sanityCheck() bool {
	//sanity check
	if token := os.Getenv(envBinCheck); token != "" {
		fmt.Fprint(os.Stdout, token)
		return true
	}
	//legacy sanity check using old env var
	if token := os.Getenv(envBinCheckLegacy); token != "" {
		fmt.Fprint(os.Stdout, token)
		return true
	}
	return false
}

// SanityCheck manually runs the check to ensure this binary
// is compatible with selfup. This tries to ensure that a restart
// is never performed against a bad binary, as it would require
// manual intervention to rectify. This is automatically done
// on selfup.Run() though it can be manually run prior whenever
// necessary.
func SanityCheck() {
	if sanityCheck() {
		os.Exit(0)
	}
}

// abstraction over master/slave
var currentProcess interface {
	triggerRestart()
	run() error
}

func runErr(c *Config) error {
	//os not supported
	if !supported {
		return fmt.Errorf("os (%s) not supported", runtime.GOOS)
	}
	if err := validate(c); err != nil {
		return err
	}
	if sanityCheck() {
		return nil
	}
	//run either in master or slave mode
	if os.Getenv(envIsSlave) == "1" {
		currentProcess = &slave{Config: c}
	} else {
		currentProcess = &master{Config: c}
	}
	return currentProcess.run()
}

// Restart programmatically triggers a graceful restart. If NoRestart
// is enabled, then this will essentially be a graceful shutdown.
func Restart() {
	if currentProcess != nil {
		currentProcess.triggerRestart()
	}
}

// IsSupported returns whether selfup is supported on the current OS.
func IsSupported() bool {
	return supported
}
