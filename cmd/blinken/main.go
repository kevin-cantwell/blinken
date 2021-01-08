package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log"
	"net"
	"os"
	"runtime/debug"
	"time"

	"github.com/disintegration/imaging"
	"github.com/kevin-cantwell/dotmatrix"
	"github.com/nfnt/resize"
)

var (
	port      = flag.String("port", "3000", "Port to serve.")
	inputFile = flag.String("input", "", "MJPEG source file.")
	ss        = flag.Int("ss", 120, "Start seconds")
	fps       = flag.Int("fps", 12, "Frames per second")
)

func main() {
	flag.Parse()

	f, err := os.Open(*inputFile)
	if err != nil {
		exit(err)
	}
	defer f.Close()

	l, err := net.Listen("tcp", ":"+*port)
	if nil != err {
		exit(err)
	}
	defer l.Close()

	ctx := context.Background()

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Println("Could not accept connection:", err)
			continue
		}
		log.Println("New connection:", conn.RemoteAddr().String())

		go func() {
			defer conn.Close()
			w, h, err := negotiateTelnet(conn)
			if err != nil {
				log.Println("telnet negotiation failed:", err)
				return
			}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			go func() {
				defer cancel()
				if err := handleCtrlC(ctx, conn); err != nil {
					log.Println(err)
				}
			}()

			handleWriter(ctx, conn, &concurrentFileReader{f: f}, w, h)
		}()
	}
}

func negotiateTelnet(rw io.ReadWriter) (int, int, error) {
	if err := negotiateUnbufferedKeystrokes(rw); err != nil {
		return 0, 0, err
	}
	w, h := negotiateWindowSize(rw)
	return int(w), int(h), nil
}

func command(rw io.ReadWriter, send []byte, handle func(resp []byte) error) error {
	if _, err := rw.Write(send); err != nil {
		return err
	}
	resp := make([]byte, 256)
	n, err := rw.Read(resp)
	if err != nil {
		return err
	}
	return handle(resp[:n])
}

func assert(rw io.ReadWriter, input, output []byte) error {
	return command(rw, input, func(resp []byte) error {
		return expect(output, resp)
	})
}

func negotiateUnbufferedKeystrokes(rw io.ReadWriter) error {
	if err := assert(rw,
		[]byte{IAC, WILL, SUPPRESS_GO_AHEAD},
		[]byte{IAC, DO, SUPPRESS_GO_AHEAD},
	); err != nil {
		return err
	}
	if err := assert(rw,
		[]byte{IAC, WILL, ECHO},
		[]byte{IAC, DO, ECHO},
	); err != nil {
		return err
	}
	return nil
}

func negotiateWindowSize(rw io.ReadWriter) (w uint16, h uint16) {
	w, h = 80, 24

	if err := command(rw, []byte{IAC, DO, NAWS}, func(resp []byte) error {
		// IAC WILL NAWS IAC SB NAWS <16-bit value> <16-bit value> IAC SE
		if len(resp) != 12 {
			return fmt.Errorf("expected response: %v", resp)
		}
		if err := expect(resp[:3], []byte{IAC, WILL, NAWS}); err != nil {
			return err
		}
		if err := expect(resp[3:6], []byte{IAC, SB, NAWS}); err != nil {
			return err
		}
		if err := expect(resp[len(resp)-2:], []byte{IAC, SE}); err != nil {
			return err
		}
		w, h = binary.BigEndian.Uint16(resp[6:8]), binary.BigEndian.Uint16(resp[8:10])
		return nil
	}); err != nil {
		log.Println(err)
		return
	}

	log.Println("window size:", w, h)

	return
}

func discardRemainder(r io.Reader) error {
	for {
		n, err := r.Read(make([]byte, 256))
		if err != nil {
			return err
		}
		if n < 256 {
			return nil
		}
	}
}

func readExpect(r io.Reader, expected ...byte) error {
	actual := make([]byte, len(expected))
	if _, err := r.Read(actual); err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) {
		return fmt.Errorf("expected read %v but was %v", expected, actual)
	}
	return nil
}

func expect(expected, actual []byte) error {
	if !bytes.Equal(actual, expected) {
		return fmt.Errorf("expected %v but was %v", expected, actual)
	}
	return nil
}

func handleCtrlC(ctx context.Context, r io.Reader) error {
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.EOF
		}
		if b[0] == CtrlC {
			return nil
		}
	}
}

func handleWriter(ctx context.Context, w io.Writer, r io.Reader, width, height int) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			debug.PrintStack()
		}
	}()

	hideCursor(w)
	defer showCursor(w)

	p := dotmatrix.NewPrinter(w, &dotmatrix.Config{
		Filter: &Filter{
			Invert:   true,
			Contrast: 25,
			Width:    width,
			Height:   height,
		},
		Flusher: &BrailleFlusher{},
	})

	timer := time.NewTimer(0)
	defer timer.Stop()

	var (
		printCh = make(chan image.Image)
		out     chan image.Image
		frame   image.Image
		bufr    = bufio.NewReader(r)
	)
	defer close(printCh)

	go func() {
		defer cancel()
		for {
			select {
			case img := <-printCh:
				if err := p.Print(img); err != nil {
					log.Println(err)
					return
				}
				rows := img.Bounds().Dy() / 4
				if img.Bounds().Dy()%4 != 0 {
					rows++
				}
				fmt.Fprintf(w, "\033[999D\033[%dA", rows)
			case <-ctx.Done():
				return
			}
		}
	}()

	readFrame := func() (io.Reader, error) {
		var buf bytes.Buffer
		for {
			token, err := bufr.ReadBytes(0xd9)
			if err != nil {
				return nil, err
			}
			if _, err := buf.Write(token); err != nil {
				return nil, err
			}
			if len(token) > 1 && bytes.Equal(token[len(token)-2:], []byte{0xff, 0xd9}) {
				return &buf, nil
			}
		}
	}

	if *ss > 0 {
		for i := 0; i < (*ss * 25); i++ {
			readFrame()
		}
	}

	m := 0

	for {
		select {
		case <-timer.C:
			timer.Reset(time.Second / 25)
			frameR, err := readFrame()
			if err != nil {
				log.Println(err)
				return
			}

			m++
			if m%2 == 0 {
				continue
			}

			img, err := jpeg.Decode(frameR)
			if err != nil {
				log.Println(err)
				return
			}
			out = printCh
			frame = img
		case out <- frame:
			out = nil
		case <-ctx.Done():
			return
		}
	}
}

func hideCursor(w io.Writer) error {
	_, err := fmt.Fprintf(w, "%s%s", "\033[?25l", "\033[40m\033[37m")
	return err
}

func showCursor(w io.Writer) error {
	_, err := fmt.Fprintf(w, "%s%s", "\033[?12l\033[?25h", "\033[0m")
	return err
}

type concurrentFileReader struct {
	f  *os.File
	at int64
}

func (r *concurrentFileReader) Read(b []byte) (int, error) {
	type _ io.ReaderAt
	n, err := r.f.ReadAt(b, r.at)
	r.at += int64(n)
	return n, err
}

func exit(err error) {
	log.Println(err)
	os.Exit(1)
}

type Filter struct {
	// Gamma less than 0 darkens the image and GAMMA greater than 0 lightens it.
	Gamma float64
	// Brightness = -100 gives solid black image. Brightness = 100 gives solid white image.
	Brightness float64
	// Contrast = -100 gives solid grey image. Contrast = 100 gives maximum contrast.
	Contrast float64
	// Sharpen greater than 0 sharpens the image.
	Sharpen float64
	// Inverts pixel color. Transparent pixels remain transparent.
	Invert bool
	// Mirror flips the image on it's vertical axis
	Mirror bool

	// Width of terminal
	Width int
	// Height of terminal
	Height int

	scale float64
}

func (f *Filter) Filter(img image.Image) image.Image {
	if f.Gamma != 0 {
		img = imaging.AdjustGamma(img, f.Gamma+1.0)
	}
	if f.Brightness != 0 {
		img = imaging.AdjustBrightness(img, f.Brightness)
	}
	if f.Sharpen != 0 {
		img = imaging.Sharpen(img, f.Sharpen)
	}
	if f.Contrast != 0 {
		// img = imaging.AdjustContrast(img, f.Contrast)
	}
	if f.Mirror {
		img = imaging.FlipH(img)
	}
	if f.Invert {
		img = imaging.Invert(img)
	}

	l := luminance(img)
	// lighten dark images
	b := (1 - l) + 1
	// img = imaging.AdjustGamma(img, b)
	// increase contrast on dark images
	c := (l)*50 + 10
	img = imaging.AdjustContrast(img, 25)
	fmt.Println("luminance:", l, "contrast:", c, "brightness:", b)

	if f.scale == 0 {
		dx, dy := img.Bounds().Dx(), img.Bounds().Dy()
		scale := scalar(dx, dy, f.Width, f.Height)
		if scale >= 1.0 {
			scale = 1.0
		}
		f.scale = scale
	}

	width := uint(f.scale * float64(img.Bounds().Dx()))
	height := uint(f.scale * float64(img.Bounds().Dy()))

	img = resize.Resize(width, height, img, resize.NearestNeighbor)

	return img
}

func scalar(dx, dy int, cols, rows int) float64 {
	scale := float64(1.0)
	scaleX := float64(cols*2) / float64(dx)
	scaleY := float64(rows*4) / float64(dy)

	if scaleX < scale {
		scale = scaleX
	}
	if scaleY < scale {
		scale = scaleY
	}

	return scale
}

func luminance(img image.Image) float64 {
	var l float64
	var max float64
	hist := imaging.Histogram(img)
	for i, p := range hist {
		if max < p {
			max = p
			l = float64(i)
		}
	}
	return 1 - (256 / l * max)
}

// special characters
const (
	IAC       byte = 255
	DONT      byte = 254
	DO        byte = 253
	WONT      byte = 252
	WILL      byte = 251
	SB        byte = 250
	GA        byte = 249
	EL        byte = 248
	EC        byte = 247
	AYT       byte = 246
	AO        byte = 245
	IP        byte = 244
	BREAK     byte = 243
	DATA_MARK byte = 242
	NOP       byte = 241
	SE        byte = 240
)

// options
const (
	ENVVARS             byte = 36
	LINEMODE            byte = 34
	REMOTE_FLOW_CONTROL byte = 33
	TERMINAL_SPEED      byte = 32
	NAWS                byte = 31 // Negotiate about window size
	TERMINAL_TYPE       byte = 24
	TIMING_MARK         byte = 6
	STATUS              byte = 5
	SUPPRESS_GO_AHEAD   byte = 3
	ECHO                byte = 1
)

// KEYSTROKES
const (
	CtrlC byte = 3
)
