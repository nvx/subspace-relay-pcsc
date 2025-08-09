package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"github.com/ansel1/merry/v2"
	"github.com/ebfe/scard"
	"github.com/eclipse/paho.golang/paho"
	"github.com/nvx/go-subspace-relay"
	subspacerelaypb "github.com/nvx/subspace-relay"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
)

// This can be set at build time using the following go build command:
// go build -ldflags="-X 'main.defaultBrokerURL=mqtts://user:pass@example.com:1234'"
var defaultBrokerURL string

func main() {
	ctx := context.Background()
	var (
		name       = flag.String("name", "", "Name of the pcsc reader to use")
		direct     = flag.Bool("direct", false, "Use direct mode")
		brokerFlag = flag.String("broker-url", "", "MQTT Broker URL")
	)
	flag.Parse()

	subspacerelay.InitLogger("subspace-relay-pcsc")

	brokerURL := subspacerelay.NotZero(*brokerFlag, os.Getenv("BROKER_URL"), defaultBrokerURL)
	if brokerURL == "" {
		slog.ErrorContext(ctx, "No broker URI specified, either specify as a flag or set the BROKER_URI environment variable")
		flag.Usage()
		os.Exit(2)
	}

	card, closer, err := connectCard(ctx, *name, *direct)
	if err != nil {
		slog.ErrorContext(ctx, "Error connecting to PCSC card", subspacerelay.ErrorAttrs(err))
		os.Exit(1)
	}
	defer closer()

	status, err := card.Status()
	if err != nil {
		slog.ErrorContext(ctx, "Error getting card status", subspacerelay.ErrorAttrs(err))
		os.Exit(1)
	}

	if len(status.Atr) > 0 {
		slog.InfoContext(ctx, "Got ATR", slog.String("atr", strings.ToUpper(hex.EncodeToString(status.Atr))))
	}

	h := &handler{
		card: card,
		clientInfo: &subspacerelaypb.ClientInfo{
			SupportedPayloadTypes: []subspacerelaypb.PayloadType{subspacerelaypb.PayloadType_PAYLOAD_TYPE_PCSC_READER, subspacerelaypb.PayloadType_PAYLOAD_TYPE_PCSC_READER_CONTROL},
			ConnectionType:        subspacerelaypb.ConnectionType_CONNECTION_TYPE_PCSC,
			Atr:                   status.Atr,
			DeviceName:            status.Reader,
		},
	}

	if *direct {
		h.clientInfo.SupportedPayloadTypes = []subspacerelaypb.PayloadType{subspacerelaypb.PayloadType_PAYLOAD_TYPE_PCSC_READER_CONTROL}
		h.clientInfo.ConnectionType = subspacerelaypb.ConnectionType_CONNECTION_TYPE_PCSC_DIRECT
	}

	m, err := subspacerelay.New(ctx, brokerURL, "")
	if err != nil {
		slog.ErrorContext(ctx, "Error connecting to server", subspacerelay.ErrorAttrs(err))
		os.Exit(1)
	}

	m.RegisterHandler(h)

	slog.InfoContext(ctx, "Connected, provide the relay_id to your friendly neighbourhood RFID hacker", slog.String("relay_id", m.RelayID))

	interruptChannel := make(chan os.Signal, 10)
	signal.Notify(interruptChannel, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(interruptChannel)

	<-interruptChannel
	err = m.Close()
	if err != nil {
		slog.ErrorContext(ctx, "Error closing client", subspacerelay.ErrorAttrs(err))
	}
}

type handler struct {
	card       *scard.Card
	clientInfo *subspacerelaypb.ClientInfo
}

func (h *handler) HandleMQTT(ctx context.Context, r *subspacerelay.SubspaceRelay, p *paho.Publish) bool {
	req, err := r.Parse(ctx, p)
	if err != nil {
		slog.ErrorContext(ctx, "Error parsing request message", subspacerelay.ErrorAttrs(err))
		return false
	}

	switch msg := req.Message.(type) {
	case *subspacerelaypb.Message_Payload:
		err = r.HandlePayload(ctx, p.Properties, msg.Payload, h.handlePayload, h.clientInfo.SupportedPayloadTypes...)
	case *subspacerelaypb.Message_RequestClientInfo:
		err = r.SendReply(ctx, p.Properties, &subspacerelaypb.Message{Message: &subspacerelaypb.Message_ClientInfo{
			ClientInfo: h.clientInfo,
		}})
	default:
		err = errors.New("unsupported message")
	}
	if err != nil {
		slog.ErrorContext(ctx, "Error handling request", subspacerelay.ErrorAttrs(err))
		return false
	}
	return true
}

func (h *handler) handlePayload(ctx context.Context, payload *subspacerelaypb.Payload) (_ []byte, err error) {
	defer subspacerelay.DeferWrap(&err)

	if payload.PayloadType == subspacerelaypb.PayloadType_PAYLOAD_TYPE_PCSC_READER_CONTROL {
		if payload.Control == nil || *payload.Control > 0xFFFF {
			err = errors.New("invalid control payload")
			return
		}
		return h.card.Control(scard.CtlCode(uint16(*payload.Control)), payload.Payload)
	}

	return h.card.Transmit(payload.Payload)
}

func connectCard(ctx context.Context, readerName string, direct bool) (_ *scard.Card, _ func(), err error) {
	defer subspacerelay.DeferWrap(&err)

	sc, err := scard.EstablishContext()
	if err != nil {
		err = merry.New("Error opening pcsc context", merry.WithCause(err))
		return
	}
	defer func() {
		if err != nil {
			_ = sc.Release()
		}
	}()

	readers, err := sc.ListReaders()
	if err != nil {
		err = merry.New("error listing readers", merry.WithCause(err))
		return
	}

	for _, el := range readers {
		slog.InfoContext(ctx, "Reader", slog.String("reader_name", el))
	}

	if len(readers) == 0 {
		err = errors.New("no pcsc readers found")
		return
	}

	if readerName == "" {
		readerName = readers[len(readers)-1]
	} else if !slices.Contains(readers, readerName) {
		err = errors.New("pcsc reader not found")
		return
	}

	slog.InfoContext(ctx, "Connecting to reader", slog.Bool("direct", direct), slog.String("reader_name", readerName))

	var card *scard.Card
	if direct {
		card, err = sc.Connect(readerName, scard.ShareDirect, scard.ProtocolUndefined)
	} else {
		card, err = sc.Connect(readerName, scard.ShareExclusive, scard.ProtocolT1)
	}
	if err != nil {
		err = merry.Wrap(err)
		return
	}

	closer := func() {
		err := card.Disconnect(scard.ResetCard)
		if err != nil {
			slog.ErrorContext(ctx, "Error disconnecting from reader", subspacerelay.ErrorAttrs(err))
		}
		err = sc.Release()
		if err != nil {
			slog.ErrorContext(ctx, "Error releasing pcsc context", subspacerelay.ErrorAttrs(err))
		}
	}

	return card, closer, nil
}
