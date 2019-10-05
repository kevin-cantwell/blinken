package main

import "net"

type connReader struct {
	conn net.Conn
}

func (r *connReader) Read(b []byte) (int, error) {
	panic("TODO")
}
