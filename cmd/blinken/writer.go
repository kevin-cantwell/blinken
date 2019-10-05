package main

import "io"

type telnetWriter struct {
	w io.Writer
}

func (w *telnetWriter) Write(p []byte) (int, error) {
	tmp := p
	numSpecial := 0
	for _, c := range p {
		if c == IAC {
			tmp = append(tmp, IAC)
			numSpecial++
		}
		tmp = append(tmp, c)
	}
	n, err := w.w.Write(tmp)
	n = n - numSpecial
	if n < 0 {
		n = 0
	}
	return n, err
}
