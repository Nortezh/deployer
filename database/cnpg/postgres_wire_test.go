package cnpg

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Test-only PostgreSQL wire responder: real TLS, password exchange and SELECT 1;
// no postgres process, container, persistent fixture or production credential.
func testPostgresTLS(t *testing.T, password string) (string, string, []byte, *atomic.Int64) {
	t.Helper()
	return testPostgresTLSQuery(t, password, "SELECT 1", "1")
}

func testPostgresTLSQuery(t *testing.T, password, query, result string) (string, string, []byte, *atomic.Int64) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-only"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	queries := &atomic.Int64{}
	var workers sync.WaitGroup
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				var ssl [8]byte
				if _, err := io.ReadFull(conn, ssl[:]); err != nil {
					return
				}
				if binary.BigEndian.Uint32(ssl[:4]) != 8 || binary.BigEndian.Uint32(ssl[4:]) != 80877103 {
					return
				}
				if _, err := conn.Write([]byte{'S'}); err != nil {
					return
				}
				secure := tls.Server(conn, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
				if secure.Handshake() != nil {
					return
				}
				var length [4]byte
				if _, err := io.ReadFull(secure, length[:]); err != nil {
					return
				}
				n := int(binary.BigEndian.Uint32(length[:])) - 4
				if n < 4 || n > 4096 {
					return
				}
				startup := make([]byte, n)
				if _, err := io.ReadFull(secure, startup); err != nil {
					return
				}
				send := func(kind byte, body []byte) bool {
					var msg bytes.Buffer
					msg.WriteByte(kind)
					_ = binary.Write(&msg, binary.BigEndian, uint32(len(body)+4))
					msg.Write(body)
					_, err := secure.Write(msg.Bytes())
					return err == nil
				}
				if !send('R', []byte{0, 0, 0, 3}) {
					return
				}
				read := func() (byte, []byte, error) {
					var hdr [5]byte
					if _, err := io.ReadFull(secure, hdr[:]); err != nil {
						return 0, nil, err
					}
					n := int(binary.BigEndian.Uint32(hdr[1:])) - 4
					if n < 0 || n > 4096 {
						return 0, nil, io.ErrUnexpectedEOF
					}
					b := make([]byte, n)
					_, err := io.ReadFull(secure, b)
					return hdr[0], b, err
				}
				kind, value, err := read()
				if err != nil || kind != 'p' || string(value) != password+"\x00" {
					return
				}
				if !send('R', []byte{0, 0, 0, 0}) || !send('Z', []byte{'I'}) {
					return
				}
				transactionStatus := byte('I')
				catalogOnly := false
				for {
					kind, body, err := read()
					if err != nil || kind == 'X' {
						return
					}
					if kind != 'Q' {
						return
					}
					statement := strings.TrimSpace(strings.TrimRight(string(body), "\x00"))
					if statement == "BEGIN READ ONLY" || statement == "SET LOCAL search_path = pg_catalog" || statement == "ROLLBACK" {
						tag := "SET"
						switch statement {
						case "BEGIN READ ONLY":
							transactionStatus, tag = 'T', "BEGIN"
						case "ROLLBACK":
							transactionStatus, tag = 'I', "ROLLBACK"
						default:
							catalogOnly = true
						}
						if !send('C', []byte(tag+"\x00")) || !send('Z', []byte{transactionStatus}) {
							return
						}
						continue
					}
					if statement != query || query == readinessWALQuery && (transactionStatus != 'T' || !catalogOnly) {
						return
					}
					var row bytes.Buffer
					_ = binary.Write(&row, binary.BigEndian, uint16(1))
					row.WriteString("?column?\x00")
					for _, v := range []any{uint32(0), uint16(0), uint32(20), int16(8), int32(-1), uint16(0)} {
						_ = binary.Write(&row, binary.BigEndian, v)
					}
					queries.Add(1)
					var data bytes.Buffer
					_ = binary.Write(&data, binary.BigEndian, uint16(1))
					_ = binary.Write(&data, binary.BigEndian, uint32(len(result)))
					data.WriteString(result)
					if !send('T', row.Bytes()) || !send('D', data.Bytes()) || !send('C', []byte("SELECT 1\x00")) || !send('Z', []byte{transactionStatus}) {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-stopped; workers.Wait() })
	return host, port, ca, queries
}
