package route_fetcher

import (
	"os"
	"sync/atomic"
	"time"

	"github.com/cloudfoundry-incubator/routing-api"
	"github.com/cloudfoundry-incubator/routing-api/db"
	uaa_client "github.com/cloudfoundry-incubator/uaa-go-client"
	"github.com/cloudfoundry-incubator/uaa-go-client/schema"
	"github.com/cloudfoundry/dropsonde/metrics"
	"github.com/cloudfoundry/gorouter/config"
	"github.com/cloudfoundry/gorouter/registry"
	"github.com/cloudfoundry/gorouter/route"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/pivotal-golang/clock"
)

type RouteFetcher struct {
	UaaClient                          uaa_client.Client
	RouteRegistry                      registry.RegistryInterface
	FetchRoutesInterval                time.Duration
	SubscriptionRetryIntervalInSeconds int

	logger          *steno.Logger
	endpoints       []db.Route
	client          routing_api.Client
	stopEventSource int32
	eventSource     atomic.Value
	eventChannel    chan routing_api.Event

	clock clock.Clock
}

const (
	TokenFetchErrors      = "token_fetch_errors"
	SubscribeEventsErrors = "subscribe_events_errors"
	maxRetries            = 3
)

func NewRouteFetcher(logger *steno.Logger, uaaClient uaa_client.Client, routeRegistry registry.RegistryInterface,
	cfg *config.Config, client routing_api.Client, subscriptionRetryInterval int, clock clock.Clock) *RouteFetcher {
	return &RouteFetcher{
		UaaClient:                          uaaClient,
		RouteRegistry:                      routeRegistry,
		FetchRoutesInterval:                cfg.PruneStaleDropletsInterval / 2,
		SubscriptionRetryIntervalInSeconds: subscriptionRetryInterval,

		client:       client,
		logger:       logger,
		eventChannel: make(chan routing_api.Event),
		clock:        clock,
	}
}

func (r *RouteFetcher) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	close(ready)
	r.startEventCycle()

	ticker := r.clock.NewTicker(r.FetchRoutesInterval)
	for {
		select {
		case <-ticker.C():
			err := r.FetchRoutes()
			if err != nil {
				r.logger.Error(err.Error())
			}

		case e := <-r.eventChannel:
			r.HandleEvent(e)

		case <-signals:
			r.logger.Info("stopping")
			atomic.StoreInt32(&r.stopEventSource, 1)
			if es := r.eventSource.Load(); es != nil {
				err := es.(routing_api.EventSource).Close()
				if err != nil {
					r.logger.Error(err.Error())
				}
			}
			ticker.Stop()
			return nil
		}
	}
}

func (r *RouteFetcher) startEventCycle() {
	go func() {
		useCachedToken := true
		for {
			r.logger.Debug("fetching-token")
			token, err := r.UaaClient.FetchToken(!useCachedToken)
			if err != nil {
				metrics.IncrementCounter(TokenFetchErrors)
				r.logger.Error(err.Error())
			} else {
				r.logger.Debug("token-fetched-successfully")
				if atomic.LoadInt32(&r.stopEventSource) == 1 {
					return
				}
				err = r.subscribeToEvents(token)
				if err != nil && err.Error() == "unauthorized" {
					useCachedToken = false
				} else {
					useCachedToken = true
				}
				if atomic.LoadInt32(&r.stopEventSource) == 1 {
					return
				}
				time.Sleep(time.Duration(r.SubscriptionRetryIntervalInSeconds) * time.Second)
			}
		}
	}()
}

func (r *RouteFetcher) subscribeToEvents(token *schema.Token) error {
	r.client.SetToken(token.AccessToken)
	source, err := r.client.SubscribeToEventsWithMaxRetries(maxRetries)
	if err != nil {
		metrics.IncrementCounter(SubscribeEventsErrors)
		r.logger.Error(err.Error())
		return err
	}

	r.logger.Info("Successfully subscribed to event stream.")

	r.eventSource.Store(source)

	for {
		event, err := source.Next()
		if err != nil {
			metrics.IncrementCounter(SubscribeEventsErrors)
			r.logger.Error(err.Error())
			break
		}
		r.logger.Debugf("Handling event: %v", event)
		r.eventChannel <- event
	}
	return err
}

func (r *RouteFetcher) HandleEvent(e routing_api.Event) {
	eventRoute := e.Route
	uri := route.Uri(eventRoute.Route)
	endpoint := route.NewEndpoint(eventRoute.LogGuid, eventRoute.IP, uint16(eventRoute.Port), eventRoute.LogGuid, nil, eventRoute.TTL, eventRoute.RouteServiceUrl)
	switch e.Action {
	case "Delete":
		r.RouteRegistry.Unregister(uri, endpoint)
	case "Upsert":
		r.RouteRegistry.Register(uri, endpoint)
	}
}

func (r *RouteFetcher) FetchRoutes() error {
	r.logger.Debug("fetch-routes-started")
	defer r.logger.Debug("fetch-routes-completed")
	useCachedToken := true
	var err error
	var routes []db.Route
	for count := 0; count < 2; count++ {
		r.logger.Debug("fetching-token")
		token, tokenErr := r.UaaClient.FetchToken(!useCachedToken)
		if tokenErr != nil {
			metrics.IncrementCounter(TokenFetchErrors)
			return tokenErr
		}
		r.client.SetToken(token.AccessToken)
		r.logger.Debug("fetching-routes")
		routes, err = r.client.Routes()
		if err != nil {
			if err.Error() == "unauthorized" {
				useCachedToken = false
			} else {
				return err
			}
		} else {
			break
		}
	}

	if err == nil {
		r.logger.Debug("refreshing-endpoints")
		r.refreshEndpoints(routes)
	}

	return err
}

func (r *RouteFetcher) refreshEndpoints(validRoutes []db.Route) {
	r.deleteEndpoints(validRoutes)

	r.endpoints = validRoutes

	for _, aRoute := range r.endpoints {
		r.RouteRegistry.Register(
			route.Uri(aRoute.Route),
			route.NewEndpoint(
				aRoute.LogGuid,
				aRoute.IP,
				uint16(aRoute.Port),
				aRoute.LogGuid,
				nil,
				aRoute.TTL,
				aRoute.RouteServiceUrl,
			))
	}
}

func (r *RouteFetcher) deleteEndpoints(validRoutes []db.Route) {
	var diff []db.Route

	for _, curRoute := range r.endpoints {
		routeFound := false

		for _, validRoute := range validRoutes {
			if routeEquals(curRoute, validRoute) {
				routeFound = true
				break
			}
		}

		if !routeFound {
			diff = append(diff, curRoute)
			r.endpoints = r.endpoints
		}
	}

	for _, aRoute := range diff {
		r.RouteRegistry.Unregister(
			route.Uri(aRoute.Route),
			route.NewEndpoint(
				aRoute.LogGuid,
				aRoute.IP,
				uint16(aRoute.Port),
				aRoute.LogGuid,
				nil,
				aRoute.TTL,
				aRoute.RouteServiceUrl,
			))
	}
}

func routeEquals(current, desired db.Route) bool {
	if current.Route == desired.Route && current.IP == desired.IP && current.Port == desired.Port {
		return true
	}

	return false
}
