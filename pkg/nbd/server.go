package nbd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
)

const (
	nbdMagic        = uint64(0x4e42444d41474943)
	iHaveOpt        = uint64(0x49484156454f5054)
	requestMagic    = uint32(0x25609513)
	replyMagic      = uint32(0x67446698)
	optExportName   = uint32(1)
	optAbort        = uint32(2)
	optGo           = uint32(7)
	repMagic        = uint64(0x3e889045565a9)
	repAck          = uint32(1)
	repInfo         = uint32(3)
	repErrUnsup     = uint32(0x80000001)
	infoExport      = uint16(0)
	flagFixedNew    = uint16(1)
	flagNoZeroes    = uint16(2)
	transHasFlags   = uint16(1)
	transSendFlush  = uint16(4)
	transSendFUA    = uint16(8)
	cmdRead         = uint16(0)
	cmdWrite        = uint16(1)
	cmdDisconnect   = uint16(2)
	cmdFlush        = uint16(3)
	maxRequestBytes = 32 * 1024 * 1024
)

type Device interface {
	Size() int64
	ReadAt(ctx context.Context, offset, length int64) ([]byte, error)
	WriteAt(ctx context.Context, offset int64, data []byte) error
	Flush(ctx context.Context) error
	Disconnect(ctx context.Context) error
}

type ReadAtIntoDevice interface {
	ReadAtInto(ctx context.Context, offset int64, dst []byte) (int, error)
}

type DeviceResolver func(exportName string) (Device, error)

type Server struct {
	Device  Device
	Resolve DeviceResolver
	Logger  *log.Logger
	Stats   *StatsCollector
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			if err := s.Handle(ctx, conn); err != nil && !errors.Is(err, io.EOF) {
				s.logf("nbd connection closed with error: %v", err)
			}
		}()
	}
}

func (s *Server) Handle(ctx context.Context, conn net.Conn) error {
	defer conn.Close()
	device, err := s.handshake(conn)
	if err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		keepGoing, err := s.handleRequest(ctx, conn, device)
		if err != nil {
			return err
		}
		if !keepGoing {
			return nil
		}
	}
}

func (s *Server) handshake(rw io.ReadWriter) (Device, error) {
	var header [18]byte
	binary.BigEndian.PutUint64(header[0:8], nbdMagic)
	binary.BigEndian.PutUint64(header[8:16], iHaveOpt)
	binary.BigEndian.PutUint16(header[16:18], flagFixedNew|flagNoZeroes)
	if _, err := rw.Write(header[:]); err != nil {
		return nil, err
	}

	var clientFlags [4]byte
	if _, err := io.ReadFull(rw, clientFlags[:]); err != nil {
		return nil, err
	}
	clientNoZeroes := binary.BigEndian.Uint32(clientFlags[:])&uint32(flagNoZeroes) != 0

	for {
		var optHeader [16]byte
		if _, err := io.ReadFull(rw, optHeader[:]); err != nil {
			return nil, err
		}
		if got := binary.BigEndian.Uint64(optHeader[0:8]); got != iHaveOpt {
			return nil, fmt.Errorf("invalid option magic 0x%x", got)
		}
		opt := binary.BigEndian.Uint32(optHeader[8:12])
		optLen := binary.BigEndian.Uint32(optHeader[12:16])
		if optLen > 64*1024 {
			return nil, fmt.Errorf("NBD option payload too large: %d", optLen)
		}
		payload := make([]byte, int(optLen))
		if optLen > 0 {
			if _, err := io.ReadFull(readerOnly{rw}, payload); err != nil {
				return nil, err
			}
		}

		switch opt {
		case optExportName:
			device, err := s.resolveDevice(string(payload))
			if err != nil {
				return nil, err
			}
			return s.writeExport(rw, device, clientNoZeroes)
		case optGo:
			device, err := s.handleOptGo(rw, opt, payload)
			if err != nil {
				return nil, err
			}
			return device, nil
		case optAbort:
			return nil, io.EOF
		default:
			if err := writeOptionReply(rw, opt, repErrUnsup, nil); err != nil {
				return nil, err
			}
		}
	}
}

func (s *Server) writeExport(rw io.ReadWriter, device Device, clientNoZeroes bool) (Device, error) {
	var export [10]byte
	binary.BigEndian.PutUint64(export[0:8], uint64(device.Size()))
	binary.BigEndian.PutUint16(export[8:10], transHasFlags|transSendFlush|transSendFUA)
	if _, err := rw.Write(export[:]); err != nil {
		return nil, err
	}
	if !clientNoZeroes {
		if _, err := rw.Write(make([]byte, 124)); err != nil {
			return nil, err
		}
	}
	return device, nil
}

func (s *Server) handleOptGo(rw io.ReadWriter, opt uint32, payload []byte) (Device, error) {
	if len(payload) < 6 {
		return nil, fmt.Errorf("invalid NBD_OPT_GO payload length %d", len(payload))
	}
	nameLen := binary.BigEndian.Uint32(payload[0:4])
	if nameLen > 4096 || int(4+nameLen+2) > len(payload) {
		return nil, fmt.Errorf("invalid NBD_OPT_GO export name length %d", nameLen)
	}
	exportName := string(payload[4 : 4+nameLen])
	device, err := s.resolveDevice(exportName)
	if err != nil {
		return nil, err
	}

	var info bytes.Buffer
	var infoHeader [2]byte
	binary.BigEndian.PutUint16(infoHeader[:], infoExport)
	info.Write(infoHeader[:])
	var export [10]byte
	binary.BigEndian.PutUint64(export[0:8], uint64(device.Size()))
	binary.BigEndian.PutUint16(export[8:10], transHasFlags|transSendFlush|transSendFUA)
	info.Write(export[:])
	if err := writeOptionReply(rw, opt, repInfo, info.Bytes()); err != nil {
		return nil, err
	}
	if err := writeOptionReply(rw, opt, repAck, nil); err != nil {
		return nil, err
	}
	return device, nil
}

func (s *Server) handleRequest(ctx context.Context, rw io.ReadWriter, device Device) (bool, error) {
	var header [28]byte
	if _, err := io.ReadFull(rw, header[:]); err != nil {
		return false, err
	}
	if got := binary.BigEndian.Uint32(header[0:4]); got != requestMagic {
		return false, fmt.Errorf("invalid request magic 0x%x", got)
	}
	cmd := binary.BigEndian.Uint16(header[6:8])
	handle := binary.BigEndian.Uint64(header[8:16])
	offset := int64(binary.BigEndian.Uint64(header[16:24]))
	length := int64(binary.BigEndian.Uint32(header[24:28]))
	if length > maxRequestBytes {
		return false, fmt.Errorf("request too large: %d", length)
	}
	if device == nil {
		return false, fmt.Errorf("missing NBD device")
	}
	if offset < 0 || length < 0 || offset+length > device.Size() {
		if cmd == cmdWrite && length > 0 {
			_, _ = io.CopyN(io.Discard, readerOnly{rw}, length)
		}
		return true, writeReply(rw, handle, 22, nil)
	}

	switch cmd {
	case cmdRead:
		s.logf("nbd read offset=%d length=%d", offset, length)
		if s.Stats != nil {
			s.Stats.RecordRead(offset, length)
		}
		return true, s.writeReadReply(ctx, rw, device, handle, offset, length)
	case cmdWrite:
		s.logf("nbd write offset=%d length=%d", offset, length)
		if s.Stats != nil {
			s.Stats.RecordWrite(length)
		}
		data := make([]byte, int(length))
		if _, err := io.ReadFull(rw, data); err != nil {
			return false, err
		}
		if err := device.WriteAt(ctx, offset, data); err != nil {
			return true, writeReply(rw, handle, 5, nil)
		}
		return true, writeReply(rw, handle, 0, nil)
	case cmdFlush:
		s.logf("nbd flush")
		if s.Stats != nil {
			s.Stats.RecordFlush()
		}
		if err := device.Flush(ctx); err != nil {
			return true, writeReply(rw, handle, 5, nil)
		}
		return true, writeReply(rw, handle, 0, nil)
	case cmdDisconnect:
		s.logf("nbd disconnect")
		if s.Stats != nil {
			s.Stats.RecordDisconnect()
		}
		if err := device.Disconnect(ctx); err != nil {
			return false, err
		}
		return false, nil
	default:
		return true, writeReply(rw, handle, 22, nil)
	}
}

func (s *Server) writeReadReply(ctx context.Context, rw io.Writer, device Device, handle uint64, offset, length int64) error {
	if into, ok := device.(ReadAtIntoDevice); ok {
		buf := getBuffer(int(length))
		defer putBuffer(buf)
		n, err := into.ReadAtInto(ctx, offset, buf[:int(length)])
		if err != nil {
			return writeReply(rw, handle, 5, nil)
		}
		if n < int(length) {
			clear(buf[n:int(length)])
			n = int(length)
		}
		return writeReply(rw, handle, 0, buf[:n])
	}

	data, err := device.ReadAt(ctx, offset, length)
	if err != nil {
		return writeReply(rw, handle, 5, nil)
	}
	return writeReply(rw, handle, 0, data)
}

func (s *Server) resolveDevice(exportName string) (Device, error) {
	if s.Resolve != nil {
		return s.Resolve(exportName)
	}
	if s.Device == nil {
		return nil, fmt.Errorf("missing NBD device")
	}
	return s.Device, nil
}

func writeReply(w io.Writer, handle uint64, errno uint32, data []byte) error {
	var reply [16]byte
	binary.BigEndian.PutUint32(reply[0:4], replyMagic)
	binary.BigEndian.PutUint32(reply[4:8], errno)
	binary.BigEndian.PutUint64(reply[8:16], handle)
	if _, err := w.Write(reply[:]); err != nil {
		return err
	}
	if len(data) > 0 {
		_, err := w.Write(data)
		return err
	}
	return nil
}

func writeOptionReply(w io.Writer, opt, rep uint32, data []byte) error {
	var reply [20]byte
	binary.BigEndian.PutUint64(reply[0:8], repMagic)
	binary.BigEndian.PutUint32(reply[8:12], opt)
	binary.BigEndian.PutUint32(reply[12:16], rep)
	binary.BigEndian.PutUint32(reply[16:20], uint32(len(data)))
	if _, err := w.Write(reply[:]); err != nil {
		return err
	}
	if len(data) > 0 {
		_, err := w.Write(data)
		return err
	}
	return nil
}

func (s *Server) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf(format, args...)
	}
}

type readerOnly struct {
	io.Reader
}

var readBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 128*1024)
		return &buf
	},
}

func getBuffer(length int) []byte {
	raw := readBufferPool.Get().(*[]byte)
	buf := *raw
	if cap(buf) < length {
		buf = make([]byte, length)
	}
	return buf[:length]
}

func putBuffer(buf []byte) {
	if cap(buf) > maxRequestBytes {
		return
	}
	buf = buf[:cap(buf)]
	readBufferPool.Put(&buf)
}
