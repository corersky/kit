package consul

import (
	"fmt"
	"io"

	consul "github.com/hashicorp/consul/api"

	"github.com/go-kit/kit/endpoint"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/sd"
	"github.com/go-kit/kit/sd/internal/cache"
	"github.com/go-kit/kit/service"
)

const defaultIndex = 0

// Subscriber yields endpoints for a service in Consul. Updates to the service
// are watched and will update the Subscriber endpoints.
type Subscriber struct {
	cache       *cache.Cache
	client      Client
	logger      log.Logger
	service     string
	tags        []string
	passingOnly bool
	endpointsc  chan []endpoint.Endpoint
	quitc       chan struct{}
}

var _ sd.Subscriber = &Subscriber{}

// NewSubscriber returns a Consul subscriber which returns Endpoints for the
// requested service. It only returns instances for which all of the passed tags
// are present.
func NewSubscriber(client Client, factory sd.Factory, logger log.Logger, service string, tags []string, passingOnly bool) (*Subscriber, error) {
	s := &Subscriber{
		cache:       cache.New(factory, logger),
		client:      client,
		logger:      log.NewContext(logger).With("service", service, "tags", fmt.Sprint(tags)),
		service:     service,
		tags:        tags,
		passingOnly: passingOnly,
		quitc:       make(chan struct{}),
	}

	instances, index, err := s.getInstances(defaultIndex, nil)
	if err == nil {
		s.logger.Log("instances", len(instances))
	} else {
		s.logger.Log("err", err)
	}

	s.cache.Update(instances)
	go s.loop(index)
	return s, nil
}

// Services implements the Subscriber interface.
func (s *Subscriber) Services() ([]service.Service, error) {
	return s.cache.Services(), nil
}

// Stop terminates the subscriber.
func (s *Subscriber) Stop() {
	close(s.quitc)
}

func (s *Subscriber) loop(lastIndex uint64) {
	var (
		instances []string
		err       error
	)
	for {
		instances, lastIndex, err = s.getInstances(lastIndex, s.quitc)
		switch {
		case err == io.EOF:
			return // stopped via quitc
		case err != nil:
			s.logger.Log("err", err)
		default:
			s.cache.Update(instances)
		}
	}
}

func (s *Subscriber) getInstances(lastIndex uint64, interruptc chan struct{}) ([]string, uint64, error) {
	tag := ""
	if len(s.tags) > 0 {
		tag = s.tags[0]
	}

	type response struct {
		instances []string
		index     uint64
	}

	var (
		errc = make(chan error, 1)
		resc = make(chan response, 1)
	)

	go func() {
		entries, meta, err := s.client.Service(s.service, tag, s.passingOnly, &consul.QueryOptions{
			WaitIndex: lastIndex,
		})
		if err != nil {
			errc <- err
			return
		}
		if len(s.tags) > 1 {
			entries = filterEntries(entries, s.tags[1:]...)
		}
		resc <- response{
			instances: makeInstances(entries),
			index:     meta.LastIndex,
		}
	}()

	select {
	case err := <-errc:
		return nil, 0, err
	case res := <-resc:
		return res.instances, res.index, nil
	case <-interruptc:
		return nil, 0, io.EOF
	}
}

func filterEntries(entries []*consul.ServiceEntry, tags ...string) []*consul.ServiceEntry {
	var es []*consul.ServiceEntry

ENTRIES:
	for _, entry := range entries {
		ts := make(map[string]struct{}, len(entry.Service.Tags))
		for _, tag := range entry.Service.Tags {
			ts[tag] = struct{}{}
		}

		for _, tag := range tags {
			if _, ok := ts[tag]; !ok {
				continue ENTRIES
			}
		}
		es = append(es, entry)
	}

	return es
}

func makeInstances(entries []*consul.ServiceEntry) []string {
	instances := make([]string, len(entries))
	for i, entry := range entries {
		addr := entry.Node.Address
		if entry.Service.Address != "" {
			addr = entry.Service.Address
		}
		instances[i] = fmt.Sprintf("%s:%d", addr, entry.Service.Port)
	}
	return instances
}