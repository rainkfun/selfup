package selfup

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"

	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/rainkfun/go-kit/hash"
	"github.com/rainkfun/selfup/fetcher"
)

var (
	tmpBinPath = filepath.Join(os.TempDir(), "selfup-"+token()+extension())
	mslog      = slog.With(slog.String("process", "selfup-master"))
)

// a selfup master process
type master struct {
	*Config
	slaveID             int
	slaveCmd            *exec.Cmd
	slaveExtraFiles     []*os.File
	binPath, tmpBinPath string
	binPerms            os.FileMode
	binHash             string
	restartMux          sync.Mutex
	restarting          bool
	restartedAt         time.Time
	restarted           chan bool
	awaitingUSR1        bool
	descriptorsReleased chan bool
	signalledAt         time.Time
	printCheckUpdate    bool
}

func (mp *master) run() error {
	mslog.Debug("run")
	// TMP: set mp.binHash
	if err := mp.checkBinary(); err != nil {
		return err
	}
	if mp.Config.Fetcher != nil {
		if err := mp.Config.Fetcher.Init(); err != nil {
			mslog.Warn("fetcher init failed, fetcher disabled.", "err", err)
			mp.Config.Fetcher = nil
		}
	}
	mp.setupSignalling()
	if err := mp.retreiveFileDescriptors(); err != nil {
		return err
	}
	if mp.Config.Fetcher != nil {
		mp.printCheckUpdate = true
		mp.fetch()
		go mp.fetchLoop()
	}
	return mp.forkLoop()
}

func (mp *master) checkBinary() error {
	//get path to binary and confirm its writable
	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find binary path (%s)", err)
	}
	mp.binPath = binPath
	if info, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("failed to stat binary (%s)", err)
	} else if info.Size() == 0 {
		return fmt.Errorf("binary file is empty")
	} else {
		//copy permissions
		mp.binPerms = info.Mode()
	}
	data, err := os.ReadFile(binPath)
	if err != nil {
		return fmt.Errorf("cannot read binary (%s)", err)
	}
	digest, err := hash.XXH64SumString(data)
	if err != nil {
		return fmt.Errorf("cannot hash binary (%s)", err)
	}
	mp.binHash = digest
	//test bin<->tmpbin moves
	if mp.Config.Fetcher != nil {
		if err := move(tmpBinPath, mp.binPath); err != nil {
			return fmt.Errorf("cannot move binary (%s)", err)
		}
		if err := move(mp.binPath, tmpBinPath); err != nil {
			return fmt.Errorf("cannot move binary back (%s)", err)
		}
	}
	return nil
}

func (mp *master) setupSignalling() {
	//updater-forker comms
	mp.restarted = make(chan bool)
	mp.descriptorsReleased = make(chan bool)
	//read all master process signals
	signals := make(chan os.Signal, 1)
	signal.Notify(signals)
	go func() {
		for s := range signals {
			mp.handleSignal(s)
		}
	}()
}

func (mp *master) handleSignal(s os.Signal) {
	if s == mp.RestartSignal {
		//user initiated manual restart
		go mp.triggerRestart()
	} else if s.String() == "child exited" {
		// will occur on every restart, ignore it
	} else
	//**during a restart** a SIGUSR1 signals
	//to the master process that, the file
	//descriptors have been released
	if mp.awaitingUSR1 && s == SIGUSR1 {
		mslog.Debug("signaled, sockets ready")
		mp.awaitingUSR1 = false
		mp.descriptorsReleased <- true
	} else
	//while the slave process is running, proxy
	//all signals through
	if mp.slaveCmd != nil && mp.slaveCmd.Process != nil {
		mslog.Debug("proxy signal", "singal", s)
		mp.sendSignal(s)
	} else
	//otherwise if not running, kill on CTRL+c
	if s == os.Interrupt {
		mslog.Debug("interupt with no slave")
		os.Exit(1)
	} else {
		mslog.Debug("signal discarded, no slave process", "singal", s)
	}
}

func (mp *master) sendSignal(s os.Signal) {
	if mp.slaveCmd != nil && mp.slaveCmd.Process != nil {
		if err := mp.slaveCmd.Process.Signal(s); err != nil {
			mslog.Debug("signal failed, assuming slave process died unexpectedly", "err", err)
			os.Exit(1)
		}
	}
}

func (mp *master) retreiveFileDescriptors() error {
	mp.slaveExtraFiles = make([]*os.File, len(mp.Config.Addresses))
	for i, addr := range mp.Config.Addresses {
		a, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			return fmt.Errorf("Invalid address %s (%s)", addr, err)
		}
		l, err := net.ListenTCP("tcp", a)
		if err != nil {
			return err
		}
		f, err := l.File()
		if err != nil {
			return fmt.Errorf("Failed to retreive fd for: %s (%s)", addr, err)
		}
		if err := l.Close(); err != nil {
			return fmt.Errorf("Failed to close listener for: %s (%s)", addr, err)
		}
		mp.slaveExtraFiles[i] = f
	}
	return nil
}

// fetchLoop is run in a goroutine
func (mp *master) fetchLoop() {
	min := mp.Config.MinFetchInterval
	time.Sleep(min)
	for {
		t0 := time.Now()
		mp.fetch()
		//duration fetch of fetch
		diff := time.Now().Sub(t0)
		if diff < min {
			delay := min - diff
			//ensures at least MinFetchInterval delay.
			//should be throttled by the fetcher!
			time.Sleep(delay)
		}
	}
}

func (mp *master) fetch() {
	if mp.restarting {
		return //skip if restarting
	}
	if mp.printCheckUpdate {
		mslog.Info("checking for updates...")
	}
	binStat := &fetcher.BinStat{
		Hash: mp.binHash,
	}
	reader, err := mp.Fetcher.Fetch(binStat)
	if err != nil {
		mslog.Warn("failed to get latest version", "err", err)
		return
	}
	if reader == nil {
		if mp.printCheckUpdate {
			mslog.Info("no updates")
		}
		mp.printCheckUpdate = false
		return //fetcher has explicitly said there are no updates
	}
	mp.printCheckUpdate = true
	mslog.Debug("streaming update...")
	//optional closer
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}
	tmpBin, err := os.OpenFile(tmpBinPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		mslog.Warn("failed to open temp binary", "err", err)
		return
	}
	defer func() {
		tmpBin.Close()
		os.Remove(tmpBinPath)
	}()
	//tee off to sha1
	hash := hash.NewXXH64()
	reader = io.TeeReader(reader, hash)
	//write to a temp file
	_, err = io.Copy(tmpBin, reader)
	if err != nil {
		mslog.Warn("failed to write temp binary", "err", err)
		return
	}
	//compare hash
	s := hash.Sum64()
	digest := fmt.Sprintf("%x", s)
	if mp.binHash == digest {
		mslog.Debug("hash match - skip")
		return
	}
	//copy permissions
	if err := chmod(tmpBin, mp.binPerms); err != nil {
		mslog.Warn("failed to make temp binary executable", "err", err)
		return
	}
	if err := chown(tmpBin, uid, gid); err != nil {
		mslog.Warn("failed to change owner of binary", "err", err)
		return
	}
	if _, err := tmpBin.Stat(); err != nil {
		mslog.Warn("failed to stat temp binary", "err", err)
		return
	}
	tmpBin.Close()
	if _, err := os.Stat(tmpBinPath); err != nil {
		mslog.Warn("failed to stat temp binary by path", "err", err)
		return
	}
	if mp.Config.PreUpgrade != nil {
		if err := mp.Config.PreUpgrade(tmpBinPath); err != nil {
			mslog.Warn("user cancelled upgrade", "err", err)
			return
		}
	}
	//selfup sanity check, dont replace our good binary with a non-executable file
	tokenIn := token()
	cmd := exec.Command(tmpBinPath)
	cmd.Env = append(os.Environ(), []string{envBinCheck + "=" + tokenIn}...)
	cmd.Args = os.Args
	returned := false
	go func() {
		time.Sleep(5 * time.Second)
		if !returned {
			mslog.Warn("sanity check against fetched executable timed-out, check selfup is running")
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		}
	}()
	tokenOut, err := cmd.CombinedOutput()
	returned = true
	if err != nil {
		mslog.Warn("failed to run temp binary", "err", err, "tmp-bin-path", tmpBinPath, "output", tokenOut)
		return
	}
	if tokenIn != string(tokenOut) {
		mslog.Warn("sanity check failed")
		return
	}
	//overwrite!
	if err := overwrite(mp.binPath, tmpBinPath); err != nil {
		mslog.Warn("failed to overwrite binary", "err", err)
		return
	}
	mslog.Info("upgraded binary", "bin-hash", mp.binHash, "new-bin-hash", digest)
	mp.binHash = digest
	//binary successfully replaced
	if !mp.Config.NoRestartAfterFetch {
		mp.triggerRestart()
	}
	//and keep fetching...
	return
}

func (mp *master) triggerRestart() {
	if mp.restarting {
		mslog.Debug("already graceful restarting")
		return //skip
	} else if mp.slaveCmd == nil || mp.restarting {
		mslog.Debug("no slave process")
		return //skip
	}
	mslog.Debug("graceful restart triggered")
	mp.restarting = true
	mp.awaitingUSR1 = true
	mp.signalledAt = time.Now()
	mp.sendSignal(mp.Config.RestartSignal) //ask nicely to terminate
	select {
	case <-mp.restarted:
		//success
		mslog.Debug("restart success")
	case <-time.After(mp.TerminateTimeout):
		//times up mr. process, we did ask nicely!
		mslog.Debug("graceful timeout, forcing exit")
		mp.sendSignal(os.Kill)
	}
}

// not a real fork
func (mp *master) forkLoop() error {
	//loop, restart command
	for {
		if err := mp.fork(); err != nil {
			return err
		}
	}
}

func (mp *master) fork() error {
	mslog.Debug("starting", "bin-path", mp.binPath)
	cmd := exec.Command(mp.binPath)
	//mark this new process as the "active" slave process.
	//this process is assumed to be holding the socket files.
	mp.slaveCmd = cmd
	mp.slaveID++
	//provide the slave process with some state
	e := os.Environ()
	e = append(e, envBinID+"="+mp.binHash)
	e = append(e, envBinPath+"="+mp.binPath)
	e = append(e, envSlaveID+"="+strconv.Itoa(mp.slaveID))
	e = append(e, envIsSlave+"=1")
	e = append(e, envNumFDs+"="+strconv.Itoa(len(mp.slaveExtraFiles)))
	cmd.Env = e
	//inherit master args/stdfiles
	cmd.Args = os.Args
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	//include socket files
	cmd.ExtraFiles = mp.slaveExtraFiles
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("Failed to start slave process: %s", err)
	}
	//was scheduled to restart, notify success
	if mp.restarting {
		mp.restartedAt = time.Now()
		mp.restarting = false
		mp.restarted <- true
	}
	//convert wait into channel
	cmdwait := make(chan error)
	go func() {
		cmdwait <- cmd.Wait()
	}()
	//wait....
	select {
	case err := <-cmdwait:
		//program exited before releasing descriptors
		//proxy exit code out to master
		code := 0
		if err != nil {
			code = 1
			if exiterr, ok := err.(*exec.ExitError); ok {
				if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
					code = status.ExitStatus()
				}
			}
		}
		mslog.Debug("prog exited", "exit-code", code)
		//if a restarts are disabled or if it was an
		//unexpected crash, proxy this exit straight
		//through to the main process
		if mp.NoRestart || !mp.restarting {
			os.Exit(code)
		}
	case <-mp.descriptorsReleased:
		//if descriptors are released, the program
		//has yielded control of its sockets and
		//a parallel instance of the program can be
		//started safely. it should serve state.Listeners
		//to ensure downtime is kept at <1sec. The previous
		//cmd.Wait() will still be consumed though the
		//result will be discarded.
	}
	return nil
}

func token() string {
	buff := make([]byte, 8)
	rand.Read(buff)
	return hex.EncodeToString(buff)
}

// On Windows, include the .exe extension, noop otherwise.
func extension() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}

	return ""
}
