package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/NathanBaulch/protoc-gen-cobra/iocodec"
)

type (
	FlagBinder func(*pflag.FlagSet)
	PreDialer  func(context.Context, *[]grpc.DialOption) error
)

type Config struct {
	ServerAddr     string
	RequestFile    string
	RequestFormat  string
	ResponseFormat string
	Timeout        time.Duration
	UseEnvVars     bool
	EnvVarPrefix   string

	TLS                bool
	ServerName         string
	InsecureSkipVerify bool
	CACertFile         string
	CertFile           string
	KeyFile            string

	flagBinders []FlagBinder
	preDialers  []PreDialer
	inDecoders  map[string]iocodec.DecoderMaker
	outEncoders map[string]iocodec.EncoderMaker
}

var DefaultConfig = &Config{
	ServerAddr:     "localhost:8080",
	RequestFormat:  "json",
	ResponseFormat: "json",
	Timeout:        10 * time.Second,
	UseEnvVars:     true,

	inDecoders: map[string]iocodec.DecoderMaker{
		"json": iocodec.JSONDecoderMaker,
		"xml":  iocodec.XMLDecoderMaker,
	},
	outEncoders: map[string]iocodec.EncoderMaker{
		"json":       iocodec.JSONEncoderMaker(false),
		"prettyjson": iocodec.JSONEncoderMaker(true),
		"xml":        iocodec.XMLEncoderMaker(false),
		"prettyxml":  iocodec.XMLEncoderMaker(true),
	},
}

func NewConfig(options ...Option) *Config {
	c := *DefaultConfig
	for _, opt := range options {
		opt(&c)
	}
	return &c
}

func RegisterFlagBinder(binder FlagBinder) {
	DefaultConfig.flagBinders = append(DefaultConfig.flagBinders, binder)
}

func RegisterPreDialer(dialer PreDialer) {
	DefaultConfig.preDialers = append(DefaultConfig.preDialers, dialer)
}

func RegisterInputDecoder(format string, maker iocodec.DecoderMaker) {
	DefaultConfig.inDecoders[format] = maker
}

func RegisterOutputEncoder(format string, maker iocodec.EncoderMaker) {
	DefaultConfig.outEncoders[format] = maker
}

func (c *Config) BindFlags(fs *pflag.FlagSet) {
	fs.StringVarP(&c.ServerAddr, "server-addr", "s", c.ServerAddr, "server address in the form host:port")
	fs.StringVarP(&c.RequestFile, "request-file", "f", c.RequestFile, "client request file; use \"-\" for stdin")
	fs.StringVarP(&c.RequestFormat, "request-format", "i", c.RequestFormat, "request format ("+strings.Join(c.decoderFormats(), ", ")+")")
	fs.StringVarP(&c.ResponseFormat, "response-format", "o", c.ResponseFormat, "response format ("+strings.Join(c.encoderFormats(), ", ")+")")
	fs.DurationVar(&c.Timeout, "timeout", c.Timeout, "client connection timeout")
	fs.BoolVar(&c.TLS, "tls", c.TLS, "enable TLS")
	fs.StringVar(&c.ServerName, "tls-server-name", c.ServerName, "TLS server name override")
	fs.BoolVar(&c.InsecureSkipVerify, "tls-insecure-skip-verify", c.InsecureSkipVerify, "INSECURE: skip TLS checks")
	fs.StringVar(&c.CACertFile, "tls-ca-cert-file", c.CACertFile, "CA certificate file")
	fs.StringVar(&c.CertFile, "tls-cert-file", c.CertFile, "client certificate file")
	fs.StringVar(&c.KeyFile, "tls-key-file", c.KeyFile, "client key file")

	for _, binder := range c.flagBinders {
		binder(fs)
	}
}

func (c *Config) decoderFormats() []string {
	f := make([]string, len(c.inDecoders))
	i := 0
	for k := range c.inDecoders {
		f[i] = k
		i++
	}
	return f
}

func (c *Config) encoderFormats() []string {
	f := make([]string, len(c.outEncoders))
	i := 0
	for k := range c.outEncoders {
		f[i] = k
		i++
	}
	return f
}

func RoundTrip(ctx context.Context, cfg *Config, fn func(grpc.ClientConnInterface, iocodec.Decoder, iocodec.Encoder) error) error {
	var err error
	var in iocodec.Decoder
	if in, err = cfg.makeDecoder(); err != nil {
		return err
	}
	var out iocodec.Encoder
	if out, err = cfg.makeEncoder(); err != nil {
		return err
	}

	opts := []grpc.DialOption{grpc.WithBlock()}
	if err := cfg.dialOpts(ctx, &opts); err != nil {
		return err
	}

	if cfg.Timeout > 0 {
		var done context.CancelFunc
		ctx, done = context.WithTimeout(ctx, cfg.Timeout)
		defer done()
	}

	cc, err := grpc.DialContext(ctx, cfg.ServerAddr, opts...)
	if err != nil {
		if err == context.DeadlineExceeded {
			return fmt.Errorf("timeout dialing server: %s", cfg.ServerAddr)
		}
		return err
	}
	defer cc.Close()

	return fn(cc, in, out)
}

func (c *Config) makeDecoder() (iocodec.Decoder, error) {
	var r io.Reader
	if stat, _ := os.Stdin.Stat(); (stat.Mode()&os.ModeCharDevice) == 0 || c.RequestFile == "-" {
		r = os.Stdin
	} else if c.RequestFile != "" {
		f, err := os.Open(c.RequestFile)
		if err != nil {
			return nil, fmt.Errorf("request file: %v", err)
		}
		defer f.Close()
		if ext := strings.TrimLeft(filepath.Ext(c.RequestFile), "."); ext != "" {
			if m, ok := c.inDecoders[ext]; ok {
				return m(f), nil
			}
		}
		r = f
	} else {
		r = nil
	}

	if r == nil || c.RequestFormat == "" {
		return iocodec.NoOp, nil
	}
	if m, ok := c.inDecoders[c.RequestFormat]; !ok {
		return nil, fmt.Errorf("unknown request format: %s", c.RequestFormat)
	} else {
		return m(r), nil
	}
}

func (c *Config) makeEncoder() (iocodec.Encoder, error) {
	if c.ResponseFormat == "" {
		return iocodec.NoOp, nil
	}
	if m, ok := c.outEncoders[c.ResponseFormat]; !ok {
		return nil, fmt.Errorf("unknown response format: %s", c.ResponseFormat)
	} else {
		return m(os.Stdout), nil
	}
}

func (c *Config) dialOpts(ctx context.Context, opts *[]grpc.DialOption) error {
	if c.TLS {
		tlsConfig := &tls.Config{InsecureSkipVerify: c.InsecureSkipVerify}
		if c.CACertFile != "" {
			caCert, err := ioutil.ReadFile(c.CACertFile)
			if err != nil {
				return fmt.Errorf("ca cert: %v", err)
			}
			certPool := x509.NewCertPool()
			certPool.AppendCertsFromPEM(caCert)
			tlsConfig.RootCAs = certPool
		}
		if c.CertFile != "" {
			if c.KeyFile == "" {
				return fmt.Errorf("key file not specified")
			}
			pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
			if err != nil {
				return fmt.Errorf("cert/key: %v", err)
			}
			tlsConfig.Certificates = []tls.Certificate{pair}
		}
		if c.ServerName != "" {
			tlsConfig.ServerName = c.ServerName
		} else {
			addr, _, _ := net.SplitHostPort(c.ServerAddr)
			tlsConfig.ServerName = addr
		}
		cred := credentials.NewTLS(tlsConfig)
		*opts = append(*opts, grpc.WithTransportCredentials(cred))
	} else {
		*opts = append(*opts, grpc.WithInsecure())
	}

	for _, dialer := range c.preDialers {
		if err := dialer(ctx, opts); err != nil {
			return err
		}
	}

	return nil
}
