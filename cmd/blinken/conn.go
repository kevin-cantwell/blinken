package main

import (
	"net"
	"sync"
)

type conn struct {
	mu sync.RWMutex
	nc net.Conn
}

func (c *conn) Read(p []byte) (int, error) {
	return c.nc.Read(p)
}

func (c *conn) Write(p []byte) (int, error) {
	tmp := p
	numSpecial := 0
	for _, c := range p {
		if c == IAC {
			tmp = append(tmp, IAC)
			numSpecial++
		}
		tmp = append(tmp, c)
	}
	n, err := c.nc.Write(tmp)
	n = n - numSpecial
	if n < 0 {
		n = 0
	}
	return n, err
}

func (c *conn) halt(err error) {
	panic("TODO")
}

func (c *conn) terminalDims() (int, int) {
	panic("TODO")
}
