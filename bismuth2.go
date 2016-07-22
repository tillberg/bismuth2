package bismuth2

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/tillberg/ansi-log"
)

const maxSessionsPerContext = 5
const networkTimeout = 15 * time.Second

type Session struct {
	*exec.Cmd
}

func NewLocalSession() *Session {
	return &Session{
		Cmd: &exec.Cmd{},
	}
}

type ExecContext struct {
	Verbose bool

	mutex     sync.Mutex
	sessions  []*Session
	logger    *alog.Logger
	logPrefix string
}

func New() *ExecContext {
	ctx := &ExecContext{}
	ctx.logger = ctx.newLogger("")
	return ctx
}

func (ctx *ExecContext) lock()   { ctx.mutex.Lock() }
func (ctx *ExecContext) unlock() { ctx.mutex.Unlock() }

func (ctx *ExecContext) SetLogPrefix(prefix string) {
	ctx.lock()
	defer ctx.unlock()
	ctx.logPrefix = prefix
}

func (ctx *ExecContext) newLogger(suffix string) *alog.Logger {
	logger := alog.New(os.Stderr, "", 0)
	prefix := fmt.Sprintf("@(dim:{isodate} )")
	if len(suffix) > 0 {
		prefix = fmt.Sprintf("@(dim:{isodate} [%s] )", suffix)
	}
	logger.EnableColorTemplate()
	logger.SetPrefix(ctx.logPrefix + prefix)
	return logger
}

func (ctx *ExecContext) NewLogger(suffix string) *alog.Logger {
	ctx.lock()
	defer ctx.unlock()
	return ctx.newLogger(suffix)
}

func (ctx *ExecContext) Logger() *alog.Logger {
	ctx.lock()
	defer ctx.unlock()
	return ctx.logger
}

type SessionSetupFn func(session *Session, finished chan error) error

func (ctx *ExecContext) startSession(setupFns []SessionSetupFn, errChan chan error) {
	session := NewLocalSession()
	finished := make(chan error, len(setupFns))
	var err error
	for _, setupFn := range setupFns {
		_err := setupFn(session, finished)
		if _err != nil {
			err = _err
		}
	}
	if err != nil {
		errChan <- err
		return
	}
	err = session.Start()
	if err != nil {
		errChan <- err
		return
	}
	pid := session.Process.Pid
	if ctx.Verbose {
		ctx.logger.Printf("@(dim:$ cd) %q @(dim:&&) %q @(dim:-> pid) @(blue:%d)\n", session.Dir, session.Args, pid)
	}
	go func() {
		// We need to finish reading from stdout/stderr before calling Wait:
		// http://stackoverflow.com/questions/20134095/why-do-i-get-bad-file-descriptor-in-this-go-program-using-stderr-and-ioutil-re
		for range setupFns {
			<-finished
		}
		err = session.Wait()
		if ctx.Verbose {
			if err == nil {
				ctx.logger.Printf("@(dim:Process) @(blue:%d) @(dim:exited.)\n", pid)
			} else {
				ctx.logger.Printf("@(dim:Process) @(blue:%d) @(dim:exited:) @(warn:%v)\n", pid, err)
			}
		}
		errChan <- err
	}()
}

func (ctx *ExecContext) StartSession(setupFns ...SessionSetupFn) (errChan chan error) {
	errChan = make(chan error, 1)
	go ctx.startSession(setupFns, errChan)
	return errChan
}

func (ctx *ExecContext) ExecSession(setupFns ...SessionSetupFn) (err error) {
	errChan := ctx.StartSession(setupFns...)
	err = <-errChan
	return err
}

func (ctx *ExecContext) SessionQuoteOut(suffix string) SessionSetupFn {
	fn := func(session *Session, finished chan error) error {
		logger := ctx.newLogger(suffix)
		stdout, err := session.StdoutPipe()
		go func() {
			defer logger.Close()
			// alog.Println("SessionQuoteOut", err)
			if err != nil {
				finished <- err
			} else {
				// alog.Println("SessionQuoteOut Copy start")
				_, err := io.Copy(logger, stdout)
				// alog.Println("SessionQuoteOut Copy end", err)
				if err == io.EOF {
					finished <- nil
				} else {
					finished <- err
				}
			}
		}()
		return err
	}
	return fn
}

func (ctx *ExecContext) SessionQuoteErr(suffix string) SessionSetupFn {
	fn := func(session *Session, finished chan error) error {
		logger := ctx.newLogger(suffix)
		stderr, err := session.StderrPipe()
		go func() {
			defer logger.Close()
			// alog.Println("SessionQuoteErr", err)
			if err != nil {
				finished <- err
			} else {
				// alog.Println("SessionQuoteErr Copy start")
				_, err := io.Copy(logger, stderr)
				// alog.Println("SessionQuoteErr Copy end", err)
				if err == io.EOF {
					finished <- nil
				} else {
					finished <- err
				}
			}
		}()
		return err
	}
	return fn
}

// func SessionShell(cmd string) SessionSetupFn {
// 	fn := func(session *Session, finished chan error) error {
// 		session.SetCmdShell(cmd)
// 		go func() {
// 			finished <- nil
// 		}()
// 		return nil
// 	}
// 	return fn
// }

func SessionArgs(args ...string) SessionSetupFn {
	fn := func(session *Session, finished chan error) error {
		path, err := exec.LookPath(args[0])
		if err != nil {
			return err
		}
		session.Path = path
		session.Args = args
		go func() {
			finished <- nil
		}()
		return nil
	}
	return fn
}

func SessionCwd(cwd string) SessionSetupFn {
	fn := func(session *Session, finished chan error) error {
		session.Cmd.Dir = cwd
		go func() {
			finished <- nil
		}()
		return nil
	}
	return fn
}

type BufferCloser struct {
	bytes.Buffer
}

func (b BufferCloser) Close() error { return nil }

func copyStdoutAndErr(session *Session, stdout io.Writer, stderr io.Writer, finished chan error) error {
	myReady := make(chan error)
	copyStream := func(session *Session, getPipe func(*Session) (io.ReadCloser, error), writer io.Writer) error {
		reader, err := getPipe(session)
		go func() {
			if err != nil {
				myReady <- err
			} else {
				_, err := io.Copy(writer, reader)
				if err == io.EOF {
					myReady <- nil
				} else {
					myReady <- err
				}
			}
		}()
		return err
	}
	err1 := copyStream(session, (*Session).StdoutPipe, stdout)
	err2 := copyStream(session, (*Session).StderrPipe, stderr)
	go func() {
		var err error
		for i := 0; i < 2; i++ {
			_err := <-myReady
			if _err != nil {
				err = _err
			}
		}
		finished <- err
	}()
	if err1 != nil {
		return err1
	}
	return err2
}

func SessionBuffer() (SessionSetupFn, chan []byte) {
	bufChan := make(chan []byte, 2)
	fn := func(session *Session, finished chan error) error {
		var bufOut BufferCloser
		var bufErr BufferCloser
		myReady := make(chan error)
		err := copyStdoutAndErr(session, &bufOut, &bufErr, myReady)
		go func() {
			err := <-myReady
			go func() {
				bufChan <- bufOut.Bytes()
				go func() {
					bufChan <- bufErr.Bytes()
				}()
			}()
			finished <- err
		}()
		return err
	}
	return fn, bufChan
}

func SessionPipeStdout(chanStdout chan io.Reader) SessionSetupFn {
	return func(session *Session, finished chan error) error {
		stdout, err := session.StdoutPipe()
		go func() {
			chanStdout <- stdout
			// XXX we need to catch EOFs somehow
			finished <- nil
		}()
		return err
	}
}

func SessionPipeStdin(chanStdin chan io.WriteCloser) SessionSetupFn {
	return func(session *Session, finished chan error) error {
		stdin, err := session.StdinPipe()
		go func() {
			chanStdin <- stdin
			finished <- nil
		}()
		return err
	}
}

// func SessionSetStdin(reader io.Reader) SessionSetupFn {
// 	return func(session *Session, finished chan error) error {
// 		session.SetStdin(reader)
// 		go func() {
// 			finished <- nil
// 		}()
// 		return nil
// 	}
// }

// func SessionInteractive() SessionSetupFn {
// 	return func(session *Session, finished chan error) error {
// 		session.SetStdin(os.Stdin)
// 		err := copyStdoutAndErr(session, os.Stdout, os.Stderr, finished)
// 		return err
// 	}
// }

// func (ctx *ExecContext) QuoteCwdPipeOut(suffix string, cwd string, chanStdout chan io.Reader, args ...string) (err error) {
// 	return ctx.ExecSession(ctx.SessionQuoteErr(suffix), SessionPipeStdout(chanStdout), SessionCwd(ctx.AbsPath(cwd)), SessionArgs(args...))
// }

// func (ctx *ExecContext) QuoteCwdPipeIn(suffix string, cwd string, chanStdin chan io.WriteCloser, args ...string) (err error) {
// 	return ctx.ExecSession(ctx.SessionQuoteOut(suffix), ctx.SessionQuoteErr(suffix), SessionPipeStdin(chanStdin), SessionCwd(ctx.AbsPath(cwd)), SessionArgs(args...))
// }

// func (ctx *ExecContext) QuoteCwdPipeInOut(suffix string, cwd string, chanStdin chan io.WriteCloser, chanStdout chan io.Reader, args ...string) (err error) {
// 	return ctx.ExecSession(ctx.SessionQuoteErr(suffix), SessionPipeStdin(chanStdin), SessionPipeStdout(chanStdout), SessionCwd(ctx.AbsPath(cwd)), SessionArgs(args...))
// }

// func (ctx *ExecContext) ShellInteractive(s string) (err error) {
// 	return ctx.ExecSession(SessionShell(s), SessionInteractive())
// }

// func (ctx *ExecContext) QuoteShell(suffix string, s string) (err error) {
// 	return ctx.ExecSession(SessionShell(s), ctx.SessionQuoteOut(suffix), ctx.SessionQuoteErr(suffix))
// }

// func (ctx *ExecContext) QuoteCwdBuf(suffix string, cwd string, args ...string) (stdout []byte, stderr []byte, err error) {
// 	bufSetup, bufChan := SessionBuffer()
// 	err = ctx.ExecSession(SessionCwd(ctx.AbsPath(cwd)), SessionArgs(args...), bufSetup, ctx.SessionQuoteOut(suffix), ctx.SessionQuoteErr(suffix))
// 	stdout = <-bufChan
// 	stderr = <-bufChan
// 	return stdout, stderr, err
// }

func (ctx *ExecContext) QuoteCwd(suffix string, cwd string, args ...string) (err error) {
	return ctx.ExecSession(SessionCwd(ctx.AbsPath(cwd)), SessionArgs(args...), ctx.SessionQuoteOut(suffix), ctx.SessionQuoteErr(suffix))
}

// func (ctx *ExecContext) QuoteDaemonCwdPipeOut(suffix string, cwd string, chanStdout chan io.Reader, args ...string) err error {
// 	return ctx.StartSession(SessionCwd(ctx.AbsPath(cwd)), SessionArgs(args...), ctx.SessionQuoteErr(suffix), SessionPipeStdout(chanStdout))
// }

// func (ctx *ExecContext) QuoteDaemonCwd(suffix string, cwd string, args ...string) (pid int, err error) {
// 	return ctx.StartSession(SessionCwd(ctx.AbsPath(cwd)), SessionArgs(args...), ctx.SessionQuoteOut(suffix), ctx.SessionQuoteErr(suffix))
// }

func (ctx *ExecContext) Quote(suffix string, args ...string) (err error) {
	err = ctx.ExecSession(SessionArgs(args...), ctx.SessionQuoteOut(suffix), ctx.SessionQuoteErr(suffix))
	return err
}

// func (ctx *ExecContext) RunShell(s string) (stdout []byte, stderr []byte, err error) {
// 	bufSetup, bufChan := SessionBuffer()
// 	err = ctx.ExecSession(bufSetup, SessionShell(s))
// 	if err != nil {
// 		return nil, nil, -1, err
// 	}
// 	stdout = <-bufChan
// 	stderr = <-bufChan
// 	return stdout, stderr, err
// }

func (ctx *ExecContext) RunCwd(cwd string, args ...string) (stdout []byte, stderr []byte, err error) {
	bufSetup, bufChan := SessionBuffer()
	err = ctx.ExecSession(bufSetup, SessionCwd(ctx.AbsPath(cwd)), SessionArgs(args...))
	if err != nil {
		return nil, nil, err
	}
	stdout = <-bufChan
	stderr = <-bufChan
	return stdout, stderr, err
}

func (ctx *ExecContext) Run(args ...string) (stdout []byte, stderr []byte, err error) {
	bufSetup, bufChan := SessionBuffer()
	err = ctx.ExecSession(bufSetup, SessionArgs(args...))
	if err != nil {
		return nil, nil, err
	}
	stdout = <-bufChan
	stderr = <-bufChan
	return stdout, stderr, err
}

// func (ctx *ExecContext) OutputShell(s string) (stdout string, err error) {
// 	bufSetup, bufChan := SessionBuffer()
// 	_, err = ctx.ExecSession(bufSetup, SessionShell(s))
// 	if err != nil {
// 		return "", err
// 	}
// 	stdout = strings.TrimSpace(string(<-bufChan))
// 	<-bufChan // ignore stderr
// 	return stdout, err
// }

// func (ctx *ExecContext) OutputCwd(cwd string, args ...string) (stdout string, err error) {
// 	bufSetup, bufChan := SessionBuffer()
// 	_, err = ctx.ExecSession(bufSetup, SessionCwd(ctx.AbsPath(cwd)), SessionArgs(args...))
// 	if err != nil {
// 		return "", err
// 	}
// 	stdout = strings.TrimSpace(string(<-bufChan))
// 	<-bufChan // ignore stderr
// 	return stdout, err
// }

// func (ctx *ExecContext) Output(args ...string) (stdout string, err error) {
// 	bufSetup, bufChan := SessionBuffer()
// 	_, err = ctx.ExecSession(bufSetup, SessionArgs(args...))
// 	if err != nil {
// 		return "", err
// 	}
// 	stdout = strings.TrimSpace(string(<-bufChan))
// 	<-bufChan // ignore stderr
// 	return stdout, err
// }

func (ctx *ExecContext) AbsPath(p string) string {
	absp, err := filepath.Abs(p)
	alog.BailIf(err)
	return absp
}
