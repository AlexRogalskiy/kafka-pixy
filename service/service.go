package service

import (
	"fmt"
	"sync"

	"github.com/mailgun/kafka-pixy/actor"
	"github.com/mailgun/kafka-pixy/admin"
	"github.com/mailgun/kafka-pixy/apiserver"
	"github.com/mailgun/kafka-pixy/config"
	"github.com/mailgun/kafka-pixy/consumer"
	"github.com/mailgun/kafka-pixy/consumer/consumerimpl"
	"github.com/mailgun/kafka-pixy/producer"
	"github.com/mailgun/log"
)

type T struct {
	actorID    *actor.ID
	prod       *producer.T
	cons       consumer.T
	admin      *admin.T
	tcpServer  *apiserver.T
	unixServer *apiserver.T
	quitCh     chan struct{}
	wg         sync.WaitGroup
}

func Spawn(cfg *config.T) (*T, error) {
	prod, err := producer.Spawn(actor.RootID, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to spawn producer, err=(%s)", err)
	}
	cons, err := consumerimpl.Spawn(actor.RootID, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to spawn consumer, err=(%s)", err)
	}
	admin, err := admin.Spawn(actor.RootID, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to spawn admin, err=(%s)", err)
	}
	tcpServer, err := apiserver.New(apiserver.NetworkTCP, cfg.TCPAddr, prod, cons, admin)
	if err != nil {
		prod.Stop()
		return nil, fmt.Errorf("failed to start TCP socket based HTTP API, err=(%s)", err)
	}
	var unixServer *apiserver.T
	if cfg.UnixAddr != "" {
		unixServer, err = apiserver.New(apiserver.NetworkUnix, cfg.UnixAddr, prod, cons, admin)
		if err != nil {
			prod.Stop()
			return nil, fmt.Errorf("failed to start Unix socket based HTTP API, err=(%s)", err)
		}
	}
	s := &T{
		actorID:    actor.RootID.NewChild("service"),
		prod:       prod,
		cons:       cons,
		admin:      admin,
		tcpServer:  tcpServer,
		unixServer: unixServer,
		quitCh:     make(chan struct{}),
	}
	actor.Spawn(s.actorID, &s.wg, s.run)
	return s, nil
}

func (s *T) Stop() {
	close(s.quitCh)
	s.wg.Wait()
}

// supervisor takes care of the service graceful shutdown.
func (s *T) run() {
	var unixServerErrorCh <-chan error

	s.tcpServer.Start()
	if s.unixServer != nil {
		s.unixServer.Start()
		unixServerErrorCh = s.unixServer.ErrorCh()
	}
	// Block to wait for quit signal or an API server crash.
	select {
	case <-s.quitCh:
	case err, ok := <-s.tcpServer.ErrorCh():
		if ok {
			log.Errorf("Unix socket based HTTP API crashed, err=(%s)", err)
		}
	case err, ok := <-unixServerErrorCh:
		if ok {
			log.Errorf("TCP socket based HTTP API crashed, err=(%s)", err)
		}
	}
	// Initiate stop of all API servers.
	s.tcpServer.AsyncStop()
	if s.unixServer != nil {
		s.unixServer.AsyncStop()
	}
	// Wait until all API servers are stopped.
	for range s.tcpServer.ErrorCh() {
		// Drain the errors channel until it is closed.
	}
	if s.unixServer != nil {
		for range s.unixServer.ErrorCh() {
			// Drain the errors channel until it is closed.
		}
	}
	// There are no more requests in flight at this point so it is safe to stop
	// all Kafka clients.
	var wg sync.WaitGroup
	actor.Spawn(s.actorID.NewChild("producerStopper"), &wg, s.prod.Stop)
	actor.Spawn(s.actorID.NewChild("consumerStopper"), &wg, s.cons.Stop)
	actor.Spawn(s.actorID.NewChild("adminStopper"), &wg, s.admin.Stop)
	wg.Wait()
}
