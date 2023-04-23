package rabbitmqstore

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type MessageHandler func(amqp091.Delivery)

type Store interface {
	RegisterListener(RegisterListenerOpts) (Listener, error)
	CloseListener(id string)
	GetListeners() map[string]Listener

	// Declares a list of exchanges. Useful for initializing the exchanges that the store will use.
	DeclareExchanges([]DeclareExchangeOpts) error

	CloseAll() error

	// Retrieves the channel. But you should most likely not use this directly.
	// You already have access to publishing and consuming messages through the parent struct.
	// This is only for fringe cases where the basic Store functionality is not enough.
	GetChannel() *amqp091.Channel

	Publish(PublishOpts) error
}

type rabbitmqStore struct {
	mutex     sync.Mutex
	logger    *zap.Logger
	conn      *amqp091.Connection
	channel   *amqp091.Channel
	listeners map[string]*listener
}

type Options struct {
	// Required if Connection is not provided.
	URL string

	// Required if URL is not provided.
	Connection *amqp091.Connection

	LoggerOpts LoggerOpts
}

type LoggerOpts struct {
	Logger   *zap.Logger
	Encoding string
	LogLevel *zapcore.Level
}

const (
	DEFAULT_LOG_LEVEL    = zapcore.WarnLevel
	DEFAULT_LOG_ENCODING = "json"
)

func New(opts Options) (Store, error) {
	var conn *amqp091.Connection = opts.Connection
	var err error

	if conn == nil {
		conn, err = amqp091.Dial(opts.URL)
		if err != nil {
			return nil, err
		}
	}

	channel, err := conn.Channel()
	if err != nil {
		return nil, err
	}

	loggerOpts := opts.LoggerOpts
	logger := loggerOpts.Logger
	if logger == nil {
		zapLevel := zap.NewAtomicLevel()

		level := func() zapcore.Level {
			if opts.LoggerOpts.LogLevel != nil {
				return *opts.LoggerOpts.LogLevel
			}

			envLevel := os.Getenv("RABBITMQSTORE_LOG_LEVEL")
			if envLevel == "" {
				return DEFAULT_LOG_LEVEL
			}

			switch strings.ToLower(envLevel) {
			case "debug":
				return zapcore.DebugLevel
			case "info":
				return zapcore.InfoLevel
			case "warn":
				return zapcore.WarnLevel
			case "fatal":
				return zapcore.FatalLevel
			case "panic":
				return zapcore.PanicLevel
			case "dpanic":
				return zapcore.DPanicLevel
			default:
				fmt.Printf(
					"\nWARNING: Invalid Log Level passed to MagicSockets via environment variable: %s. Will use default log level: %s\n",
					envLevel,
					DEFAULT_LOG_LEVEL.String(),
				)
				return DEFAULT_LOG_LEVEL
			}
		}()

		zapLevel.SetLevel(level)

		encoding := func() string {
			if loggerOpts.Encoding == "" {
				return DEFAULT_LOG_ENCODING
			}

			return loggerOpts.Encoding
		}()

		config := zap.Config{
			Level:             zapLevel,
			Development:       false,
			DisableCaller:     true,
			DisableStacktrace: true,
			OutputPaths:       []string{"stdout"},
			ErrorOutputPaths:  []string{"stderr"},
			Encoding:          encoding,
			EncoderConfig: zapcore.EncoderConfig{
				TimeKey:        "timestamp",
				LevelKey:       "level",
				MessageKey:     "message",
				CallerKey:      "caller",
				EncodeTime:     zapcore.ISO8601TimeEncoder,
				EncodeLevel:    zapcore.LowercaseLevelEncoder,
				EncodeDuration: zapcore.StringDurationEncoder,
				EncodeCaller:   zapcore.ShortCallerEncoder,
			},
		}
		logger, err = config.Build()
		if err != nil {
			panic(fmt.Errorf("\n failed to build logger configurations for magicsockets: %s\n", err.Error()))
		}
	}
	logger = logger.With(zap.String("RabbitMQ Store ID", uuid.New().String()))

	return &rabbitmqStore{
		mutex:     sync.Mutex{},
		logger:    logger,
		conn:      conn,
		channel:   channel,
		listeners: make(map[string]*listener),
	}, nil
}

func (r *rabbitmqStore) GetChannel() *amqp091.Channel {
	return r.channel
}

func (r *rabbitmqStore) GetListeners() map[string]Listener {
	listeners := make(map[string]Listener)
	for k := range r.listeners {
		listeners[k] = r.listeners[k]
	}
	return listeners
}

func (r *rabbitmqStore) CloseAll() error {
	err := r.channel.Close()
	if err != nil {
		return err
	}

	err = r.conn.Close()
	if err != nil {
		return err
	}

	return nil
}

// Safe to call for IDs that don't exist.
func (r *rabbitmqStore) CloseListener(id string) {
	_, ok := r.listeners[id]
	if !ok {
		return
	}

	r.listeners[id].mutex.Lock()
	delete(r.listeners, id)
}

type DeclareExchangeOpts struct {
	// Required.
	Exchange string

	// Defaults to topic.
	Kind string
}

func (r *rabbitmqStore) DeclareExchanges(optsList []DeclareExchangeOpts) error {
	for i := range optsList {
		opt := optsList[i]

		if opt.Kind == "" {
			opt.Kind = "topic"
		}

		r.logger.Debug(
			"Declaring exchange",
			zap.String("Exchange", opt.Exchange),
			zap.String("Kind", opt.Kind),
		)

		if err := r.channel.ExchangeDeclare(
			opt.Exchange,
			opt.Kind,
			false,
			false,
			false,
			false,
			nil,
		); err != nil {
			return err
		}
	}

	return nil
}
