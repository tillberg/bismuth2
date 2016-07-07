package bismuth2

import "os/exec"

type Session struct {
	*exec.Cmd
}

func NewLocalSession() *Session {
	return &Session{
		Cmd: &exec.Cmd{},
	}
}

func (s *Session) Close() {

}
