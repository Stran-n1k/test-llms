// Package e2e drives the real binary over HTTP: two processes share one
// PostgreSQL, the same way two instances of the service run in production.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"booking/internal/handler/rest/gen"

	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

const (
	postgresImage = "postgres:18.1-alpine"
	readyTimeout  = 90 * time.Second
	stopTimeout   = 30 * time.Second
)

// instance is one running service process.
type instance struct {
	cmd    *exec.Cmd
	addr   string
	output *syncBuffer
	exited chan error
}

func (i *instance) url(path string) string {
	return "http://" + i.addr + path
}

// syncBuffer collects process output that a test reads on failure.
type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(data)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}

// E2ESuite shares one database and two instances. Every test creates its own
// events, so tests do not affect each other.
type E2ESuite struct {
	suite.Suite

	container   *tcpostgres.PostgresContainer
	databaseURL string
	binary      string
	first       *instance
	second      *instance
	client      *http.Client
}

// TestE2ESuite needs Docker; use go test -short to skip it.
func TestE2ESuite(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test: needs Docker")
	}

	suite.Run(t, new(E2ESuite))
}

func (s *E2ESuite) SetupSuite() {
	ctx := s.T().Context()
	s.client = &http.Client{Timeout: 30 * time.Second}

	s.binary = filepath.Join(s.T().TempDir(), "booking")
	build := exec.CommandContext(ctx, "go", "build", "-o", s.binary, "booking/cmd/booking")

	output, err := build.CombinedOutput()
	s.Require().NoError(err, "build service: %s", output)

	container, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("booking"),
		tcpostgres.WithUsername("booking"),
		tcpostgres.WithPassword("booking"),
		tcpostgres.BasicWaitStrategies(),
	)
	s.Require().NoError(err, "start PostgreSQL container; is Docker running?")

	s.container = container

	s.databaseURL, err = container.ConnectionString(ctx, "sslmode=disable")
	s.Require().NoError(err)

	// Both start at once on an empty database, so they race to prepare the schema.
	s.first = s.startInstance()
	s.second = s.startInstance()
	s.waitReady(s.first)
	s.waitReady(s.second)
}

func (s *E2ESuite) TearDownSuite() {
	s.stopInstance(s.first, syscall.SIGTERM)
	s.stopInstance(s.second, syscall.SIGTERM)

	if s.container != nil {
		s.Require().NoError(testcontainers.TerminateContainer(s.container))
	}
}

func (s *E2ESuite) TearDownTest() {
	if s.T().Failed() {
		s.T().Logf("first instance log:\n%s", s.first.output)
		s.T().Logf("second instance log:\n%s", s.second.output)
	}
}

func (s *E2ESuite) startInstance() *instance {
	s.T().Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	s.Require().NoError(err)

	addr := listener.Addr().String()
	s.Require().NoError(listener.Close())

	return s.startInstanceOn(addr)
}

func (s *E2ESuite) startInstanceOn(addr string) *instance {
	s.T().Helper()

	output := &syncBuffer{}

	//nolint:gosec // The binary is the one this suite has just built.
	cmd := exec.Command(s.binary)
	cmd.Env = append(os.Environ(), "DATABASE_URL="+s.databaseURL, "HTTP_ADDR="+addr)
	cmd.Stdout = output
	cmd.Stderr = output
	s.Require().NoError(cmd.Start())

	started := &instance{cmd: cmd, addr: addr, output: output, exited: make(chan error, 1)}

	go func() {
		started.exited <- cmd.Wait()
	}()

	return started
}

func (s *E2ESuite) waitReady(target *instance) {
	s.T().Helper()

	s.Require().Eventually(func() bool {
		response, err := s.client.Get(target.url("/healthz"))
		if err != nil {
			return false
		}

		defer response.Body.Close()

		return response.StatusCode == http.StatusOK
	}, readyTimeout, 50*time.Millisecond, "instance on %s did not become ready: %s", target.addr, target.output)
}

// stopInstance signals the process and returns its exit error.
func (s *E2ESuite) stopInstance(target *instance, signal syscall.Signal) error {
	s.T().Helper()

	if target == nil {
		return nil
	}

	s.Require().NoError(target.cmd.Process.Signal(signal))

	select {
	case err := <-target.exited:
		return err
	case <-time.After(stopTimeout):
		s.Require().NoError(target.cmd.Process.Kill())

		return fmt.Errorf("instance on %s ignored %s", target.addr, signal)
	}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (s *E2ESuite) do(ctx context.Context, method, url string, headers map[string]string, body any) (response, error) {
	var reader io.Reader

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return response{}, err
		}

		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return response{}, err
	}

	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	for name, value := range headers {
		request.Header.Set(name, value)
	}

	answer, err := s.client.Do(request)
	if err != nil {
		return response{}, err
	}
	defer answer.Body.Close()

	data, err := io.ReadAll(answer.Body)
	if err != nil {
		return response{}, err
	}

	return response{status: answer.StatusCode, header: answer.Header, body: data}, nil
}

func decode[T any](s *E2ESuite, answer response) T {
	s.T().Helper()

	var value T

	s.Require().NoError(json.Unmarshal(answer.body, &value), "body: %s", answer.body)

	return value
}

func (s *E2ESuite) createEvent(capacity int) gen.Event {
	s.T().Helper()

	answer, err := s.do(s.T().Context(), http.MethodPost, s.first.url("/events"), nil, gen.CreateEventRequest{
		Name:     "Concert",
		StartsAt: time.Date(2030, time.January, 31, 19, 0, 0, 0, time.UTC),
		Capacity: int32(capacity), //nolint:gosec // Small test values.
	})
	s.Require().NoError(err)
	s.Require().Equal(http.StatusCreated, answer.status, "body: %s", answer.body)

	return decode[gen.Event](s, answer)
}

func (s *E2ESuite) book(ctx context.Context, target *instance, eventID uuid.UUID, key string, seats int) (response, error) {
	return s.do(ctx, http.MethodPost, target.url("/events/"+eventID.String()+"/bookings"),
		map[string]string{"Idempotency-Key": key},
		gen.CreateBookingRequest{CustomerName: "Anna", Seats: int32(seats)}, //nolint:gosec // Small test values.
	)
}

func (s *E2ESuite) fetchEvent(target *instance, eventID uuid.UUID) gen.Event {
	s.T().Helper()

	answer, err := s.do(s.T().Context(), http.MethodGet, target.url("/events/"+eventID.String()), nil, nil)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, answer.status, "body: %s", answer.body)

	return decode[gen.Event](s, answer)
}

// listBookings returns every booking of the event, following pagination.
func (s *E2ESuite) listBookings(target *instance, eventID uuid.UUID) []gen.Booking {
	s.T().Helper()

	var all []gen.Booking

	for {
		url := fmt.Sprintf("%s?limit=200&offset=%d", target.url("/events/"+eventID.String()+"/bookings"), len(all))

		answer, err := s.do(s.T().Context(), http.MethodGet, url, nil, nil)
		s.Require().NoError(err)
		s.Require().Equal(http.StatusOK, answer.status, "body: %s", answer.body)

		page := decode[gen.BookingPage](s, answer)
		all = append(all, page.Items...)

		if !page.HasMore {
			return all
		}
	}
}

// inParallel runs calls at the same moment, alternating between the instances,
// and fails on any transport error.
func (s *E2ESuite) inParallel(count int, call func(ctx context.Context, target *instance, index int) (response, error)) []response {
	s.T().Helper()

	ctx := s.T().Context()
	answers := make([]response, count)
	errs := make([]error, count)
	start := make(chan struct{})

	var group sync.WaitGroup

	for index := range count {
		target := s.first
		if index%2 == 1 {
			target = s.second
		}

		group.Go(func() {
			<-start

			answers[index], errs[index] = call(ctx, target, index)
		})
	}

	close(start)
	group.Wait()

	for _, err := range errs {
		s.Require().NoError(err)
	}

	return answers
}
