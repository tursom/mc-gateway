package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/rs/zerolog/log"
)

type (
	quicConn struct {
		quic.Connection
		quic.Stream
	}
)

func runQuic(wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: config.Quic.Port})
	if err != nil {
		log.Panic().Err(err).Msg("Failed to listen UDP")
	}
	defer udpConn.Close()

	tlsConf, err := generateTLSConfig()
	if err != nil {
		log.Panic().Err(err).Msg("Failed to generate TLS config")
	}

	ln, err := quic.Listen(udpConn, tlsConf, nil)
	if err != nil {
		log.Panic().Err(err).Msg("Failed to listen QUIC")
	}
	log.Info().Int("port", config.Quic.Port).Msg("Listening for QUIC connections")

	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			log.Err(err).Msg("Error accepting QUIC connection")
			continue
		}

		log.Debug().
			Str("client", conn.RemoteAddr().String()).
			Msg("Accepted QUIC connection")
		go handleQuicRequest(conn)
	}
}

func upstreamQuic(host string) net.Conn {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true, // 跳过证书检查
		NextProtos:         getQuicNextProtos(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second) // 3s handshake timeout
	defer cancel()

	conn, err := quic.DialAddr(ctx, host, tlsConf, nil)
	if err != nil {
		gatewayMetrics.UpstreamDialError()
		log.Err(err).Str("host", host).Msg("Failed to dial QUIC")
		return nil
	}

	stream, err := conn.OpenStream()
	if err != nil {
		gatewayMetrics.UpstreamDialError()
		log.Err(err).Str("host", host).Msg("Failed to open stream")
		conn.CloseWithError(0, "failed to open stream")
		return nil
	}
	log.Debug().Str("host", host).Msg("QUIC stream opened")

	return quicConn{
		Connection: conn,
		Stream:     stream,
	}
}

func handleQuicRequest(conn quic.Connection) {
	defer conn.CloseWithError(0, "Closing connection")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		log.Err(err).Msg("Error accepting stream")
		return
	}

	handleRequest(quicConn{
		Connection: conn,
		Stream:     stream,
	})
}

func generateTLSConfig() (*tls.Config, error) {
	// 生成私钥
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	// 创建证书模板
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Example Org"},
		},
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(365 * 24 * time.Hour), // 有效期 1 年

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	// 自签名证书
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}

	// 编码证书和私钥
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	keyPEMBlock := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyPEM})

	// 加载到 tls.Certificate
	cert, err := tls.X509KeyPair(certPEM, keyPEMBlock)
	if err != nil {
		return nil, err
	}

	// 返回 tls.Config
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   getQuicNextProtos(),
	}, nil
}

func getQuicNextProtos() []string {
	nextProtos := config.Quic.ApplicationProtocols
	if len(nextProtos) == 0 {
		return []string{"minecraft", "quic", "raw", "h3"} // 默认协议
	}
	return nextProtos
}

func (c quicConn) Close() error {
	_ = c.Stream.Close()
	return c.Connection.CloseWithError(0, "Closing QUIC connection")
}

func (c quicConn) CloseWrite() error {
	return c.Stream.Close()
}

func (c quicConn) CloseRead() error {
	c.Stream.CancelRead(0)
	return nil
}
