package server

import (
	"fmt"
	"log"
	"os/exec"
	"path"
	"time"
	"warden/protocol"
	"warden/server/config"
)

type request struct {
	c    *Conn
	r    protocol.Request
	done chan bool
}

func newRequest(c_ *Conn, r_ protocol.Request) *request {
	r := &request{c: c_, r: r_}
	r.done = make(chan bool)
	return r
}

type Job struct {
}

type Container struct {
	Config *config.Config

	r chan *request
	s *Server

	State string

	Id     string
	Handle string
}

func NewContainer(s *Server, cfg *config.Config) *Container {
	c := &Container{}

	c.Config = cfg

	c.r = make(chan *request)
	c.s = s

	c.State = "born"

	c.Id = NextId()
	c.Handle = c.Id

	return c
}

func (c *Container) Execute(c_ *Conn, r_ protocol.Request) {
	r := newRequest(c_, r_)

	// Send request
	c.r <- r

	// Wait
	<-r.done
}

func (c *Container) ContainerPath() string {
	return path.Join(c.Config.Server.ContainerDepotPath, c.Handle)
}

func (c *Container) Run() {
	for r := range c.r {
		t1 := time.Now()

		switch c.State {
		case "born":
			c.runBorn(r)

		case "active":
			c.runActive(r)

		case "stopped":
			c.runStopped(r)

		case "destroyed":
			c.runDestroyed(r)

		default:
			panic("Unknown state: " + c.State)
		}

		t2 := time.Now()

		log.Printf("took: %.6fs\n", t2.Sub(t1).Seconds())
	}
}

func (c *Container) invalidState(r *request) {
	m := fmt.Sprintf("Cannot execute request in state %s", c.State)
	r.c.WriteErrorResponse(m)
}

func (c *Container) runBorn(r *request) {
	switch req := r.r.(type) {
	case *protocol.CreateRequest:
		c.DoCreate(r.c, req)
		close(r.done)

	default:
		c.invalidState(r)
		close(r.done)
	}
}

func (c *Container) runActive(r *request) {
	switch req := r.r.(type) {
	case *protocol.StopRequest:
		c.DoStop(r.c, req)
		close(r.done)

	case *protocol.DestroyRequest:
		c.DoDestroy(r.c, req)
		close(r.done)

	default:
		c.invalidState(r)
		close(r.done)
	}
}

func (c *Container) runStopped(r *request) {
	switch req := r.r.(type) {
	case *protocol.DestroyRequest:
		c.DoDestroy(r.c, req)
		close(r.done)

	default:
		c.invalidState(r)
		close(r.done)
	}
}

func (c *Container) runDestroyed(r *request) {
	switch r.r.(type) {
	default:
		c.invalidState(r)
		close(r.done)
	}
}

func runCommand(cmd *exec.Cmd) error {
	log.Printf("Run: %#v\n", cmd.Args)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Error running %s: %s\n", cmd.Args[0], err)
		log.Printf("Output: %s\n", out)
	}

	return err
}

func (c *Container) DoCreate(x *Conn, req *protocol.CreateRequest) {
	var cmd *exec.Cmd
	var err error

	// Override handle if specified
	if h := req.GetHandle(); h != "" {
		c.Handle = h
	}

	res := &protocol.CreateResponse{}
	res.Handle = &c.Handle

	// Create
	cmd = exec.Command(path.Join(c.Config.Server.ContainerScriptPath, "create.sh"), c.ContainerPath())
	cmd.Env = append(cmd.Env, fmt.Sprintf("id=%s", c.Id))
	cmd.Env = append(cmd.Env, fmt.Sprintf("network_host_ip=%s", "10.0.0.1"))
	cmd.Env = append(cmd.Env, fmt.Sprintf("network_container_ip=%s", "10.0.0.2"))
	cmd.Env = append(cmd.Env, fmt.Sprintf("user_uid=%d", 10000))
	cmd.Env = append(cmd.Env, fmt.Sprintf("rootfs_path=%s", c.Config.Server.ContainerRootfsPath))

	err = runCommand(cmd)
	if err != nil {
		x.WriteErrorResponse("error")
		return
	}

	// Start
	cmd = exec.Command(path.Join(c.ContainerPath(), "start.sh"))
	err = runCommand(cmd)
	if err != nil {
		x.WriteErrorResponse("error")
		return
	}

	c.State = "active"
	c.s.RegisterContainer(c)

	x.WriteResponse(res)
}

func (c *Container) DoStop(x *Conn, req *protocol.StopRequest) {
	var cmd *exec.Cmd

	done := make(chan error, 1)

	cmd = exec.Command(path.Join(c.ContainerPath(), "stop.sh"))

	// Don't wait for graceful stop if kill=true
	if req.GetKill() {
		cmd.Args = append(cmd.Args, "-w", "0")
	}

	// Run command in background
	go func() {
		done <- runCommand(cmd)
	}()

	// Wait for completion if background=false
	if !req.GetBackground() {
		<-done
	}

	c.State = "stopped"

	res := &protocol.StopResponse{}
	x.WriteResponse(res)
}

func (c *Container) DoDestroy(x *Conn, req *protocol.DestroyRequest) {
	var cmd *exec.Cmd
	var err error

	cmd = exec.Command(path.Join(c.ContainerPath(), "destroy.sh"))

	err = runCommand(cmd)
	if err != nil {
		x.WriteErrorResponse("error")
		return
	}

	c.State = "destroyed"
	c.s.UnregisterContainer(c)

	res := &protocol.DestroyResponse{}
	x.WriteResponse(res)
}