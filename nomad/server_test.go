package nomad

import (
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"path"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/consul/lib/freeport"
	memdb "github.com/hashicorp/go-memdb"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/command/agent/consul"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/nomad/testutil"
	"github.com/stretchr/testify/assert"
)

var (
	nodeNumber uint32 = 0
)

func testLogger() *log.Logger {
	return log.New(os.Stderr, "", log.LstdFlags)
}

func tmpDir(t *testing.T) string {
	dir, err := ioutil.TempDir("", "nomad")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	return dir
}

func testACLServer(t *testing.T, cb func(*Config)) (*Server, *structs.ACLToken) {
	server := testServer(t, func(c *Config) {
		c.ACLEnabled = true
		if cb != nil {
			cb(c)
		}
	})
	token := mock.ACLManagementToken()
	err := server.State().BootstrapACLTokens(1, 0, token)
	if err != nil {
		t.Fatalf("failed to bootstrap ACL token: %v", err)
	}
	return server, token
}

func testServer(t *testing.T, cb func(*Config)) *Server {
	// Setup the default settings
	config := DefaultConfig()
	config.Build = "0.7.0+unittest"
	config.DevMode = true
	nodeNum := atomic.AddUint32(&nodeNumber, 1)
	config.NodeName = fmt.Sprintf("nomad-%03d", nodeNum)

	// Tighten the Serf timing
	config.SerfConfig.MemberlistConfig.BindAddr = "127.0.0.1"
	config.SerfConfig.MemberlistConfig.SuspicionMult = 2
	config.SerfConfig.MemberlistConfig.RetransmitMult = 2
	config.SerfConfig.MemberlistConfig.ProbeTimeout = 50 * time.Millisecond
	config.SerfConfig.MemberlistConfig.ProbeInterval = 100 * time.Millisecond
	config.SerfConfig.MemberlistConfig.GossipInterval = 100 * time.Millisecond

	// Tighten the Raft timing
	config.RaftConfig.LeaderLeaseTimeout = 50 * time.Millisecond
	config.RaftConfig.HeartbeatTimeout = 50 * time.Millisecond
	config.RaftConfig.ElectionTimeout = 50 * time.Millisecond
	config.RaftTimeout = 500 * time.Millisecond

	// Disable Vault
	f := false
	config.VaultConfig.Enabled = &f

	// Squelch output when -v isn't specified
	if !testing.Verbose() {
		config.LogOutput = ioutil.Discard
	}

	// Invoke the callback if any
	if cb != nil {
		cb(config)
	}

	// Enable raft as leader if we have bootstrap on
	config.RaftConfig.StartAsLeader = !config.DevDisableBootstrap

	logger := log.New(config.LogOutput, fmt.Sprintf("[%s] ", config.NodeName), log.LstdFlags)
	catalog := consul.NewMockCatalog(logger)

	for i := 10; i >= 0; i-- {
		// Get random ports
		ports := freeport.GetT(t, 2)
		config.RPCAddr = &net.TCPAddr{
			IP:   []byte{127, 0, 0, 1},
			Port: ports[0],
		}
		config.SerfConfig.MemberlistConfig.BindPort = ports[1]

		// Create server
		server, err := NewServer(config, catalog, logger)
		if err == nil {
			return server
		} else if i == 0 {
			t.Fatalf("err: %v", err)
		} else {
			if server != nil {
				server.Shutdown()
			}
			wait := time.Duration(rand.Int31n(2000)) * time.Millisecond
			time.Sleep(wait)
		}
	}

	return nil
}

func testJoin(t *testing.T, s1 *Server, other ...*Server) {
	addr := fmt.Sprintf("127.0.0.1:%d",
		s1.config.SerfConfig.MemberlistConfig.BindPort)
	for _, s2 := range other {
		if num, err := s2.Join([]string{addr}); err != nil {
			t.Fatalf("err: %v", err)
		} else if num != 1 {
			t.Fatalf("bad: %d", num)
		}
	}
}

func TestServer_RPC(t *testing.T) {
	t.Parallel()
	s1 := testServer(t, nil)
	defer s1.Shutdown()

	var out struct{}
	if err := s1.RPC("Status.Ping", struct{}{}, &out); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestServer_RPC_MixedTLS(t *testing.T) {
	t.Parallel()
	const (
		cafile  = "../helper/tlsutil/testdata/ca.pem"
		foocert = "../helper/tlsutil/testdata/nomad-foo.pem"
		fookey  = "../helper/tlsutil/testdata/nomad-foo-key.pem"
	)
	dir := tmpDir(t)
	defer os.RemoveAll(dir)
	s1 := testServer(t, func(c *Config) {
		c.BootstrapExpect = 3
		c.DevMode = false
		c.DevDisableBootstrap = true
		c.DataDir = path.Join(dir, "node1")
		c.TLSConfig = &config.TLSConfig{
			EnableHTTP:           true,
			EnableRPC:            true,
			VerifyServerHostname: true,
			CAFile:               cafile,
			CertFile:             foocert,
			KeyFile:              fookey,
		}
	})
	defer s1.Shutdown()

	s2 := testServer(t, func(c *Config) {
		c.BootstrapExpect = 3
		c.DevMode = false
		c.DevDisableBootstrap = true
		c.DataDir = path.Join(dir, "node2")
	})
	defer s2.Shutdown()
	s3 := testServer(t, func(c *Config) {
		c.BootstrapExpect = 3
		c.DevMode = false
		c.DevDisableBootstrap = true
		c.DataDir = path.Join(dir, "node3")
	})
	defer s3.Shutdown()

	testJoin(t, s1, s2, s3)

	l1, l2, l3, shutdown := make(chan error, 1), make(chan error, 1), make(chan error, 1), make(chan struct{}, 1)

	wait := func(done chan error, rpc func(string, interface{}, interface{}) error) {
		for {
			select {
			case <-shutdown:
				return
			default:
			}

			args := &structs.GenericRequest{}
			var leader string
			err := rpc("Status.Leader", args, &leader)
			if err != nil || leader != "" {
				done <- err
			}
		}
	}

	go wait(l1, s1.RPC)
	go wait(l2, s2.RPC)
	go wait(l3, s3.RPC)

	select {
	case <-time.After(5 * time.Second):
	case err := <-l1:
		t.Fatalf("Server 1 has leader or error: %v", err)
	case err := <-l2:
		t.Fatalf("Server 2 has leader or error: %v", err)
	case err := <-l3:
		t.Fatalf("Server 3 has leader or error: %v", err)
	}
}

func TestServer_Regions(t *testing.T) {
	t.Parallel()
	// Make the servers
	s1 := testServer(t, func(c *Config) {
		c.Region = "region1"
	})
	defer s1.Shutdown()

	s2 := testServer(t, func(c *Config) {
		c.Region = "region2"
	})
	defer s2.Shutdown()

	// Join them together
	s2Addr := fmt.Sprintf("127.0.0.1:%d",
		s2.config.SerfConfig.MemberlistConfig.BindPort)
	if n, err := s1.Join([]string{s2Addr}); err != nil || n != 1 {
		t.Fatalf("Failed joining: %v (%d joined)", err, n)
	}

	// Try listing the regions
	testutil.WaitForResult(func() (bool, error) {
		out := s1.Regions()
		if len(out) != 2 || out[0] != "region1" || out[1] != "region2" {
			return false, fmt.Errorf("unexpected regions: %v", out)
		}
		return true, nil
	}, func(err error) {
		t.Fatalf("err: %v", err)
	})
}

func TestServer_Reload_Vault(t *testing.T) {
	t.Parallel()
	s1 := testServer(t, func(c *Config) {
		c.Region = "region1"
	})
	defer s1.Shutdown()

	if s1.vault.Running() {
		t.Fatalf("Vault client should not be running")
	}

	tr := true
	config := s1.config
	config.VaultConfig.Enabled = &tr
	config.VaultConfig.Token = uuid.Generate()

	if err := s1.Reload(config); err != nil {
		t.Fatalf("Reload failed: %v", err)
	}

	if !s1.vault.Running() {
		t.Fatalf("Vault client should be running")
	}
}

// Tests that the server will successfully reload its network connections,
// upgrading from plaintext to TLS if the server's TLS configuration changes.
func TestServer_Reload_TLSConnections_PlaintextToTLS(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	const (
		cafile  = "../helper/tlsutil/testdata/ca.pem"
		foocert = "../helper/tlsutil/testdata/nomad-foo.pem"
		fookey  = "../helper/tlsutil/testdata/nomad-foo-key.pem"
	)
	dir := tmpDir(t)
	defer os.RemoveAll(dir)
	s1 := testServer(t, func(c *Config) {
		c.DataDir = path.Join(dir, "nodeA")
	})
	defer s1.Shutdown()

	// assert that the server started in plaintext mode
	assert.Equal(s1.config.TLSConfig.CertFile, "")

	newTLSConfig := &config.TLSConfig{
		EnableHTTP:           true,
		EnableRPC:            true,
		VerifyServerHostname: true,
		CAFile:               cafile,
		CertFile:             foocert,
		KeyFile:              fookey,
	}

	err := s1.reloadTLSConnections(newTLSConfig)
	assert.Nil(err)

	assert.True(s1.config.TLSConfig.Equals(newTLSConfig))

	time.Sleep(10 * time.Second)
	codec := rpcClient(t, s1)

	node := mock.Node()
	req := &structs.NodeRegisterRequest{
		Node:         node,
		WriteRequest: structs.WriteRequest{Region: "global"},
	}

	var resp structs.GenericResponse
	err = msgpackrpc.CallWithCodec(codec, "Node.Register", req, &resp)
	assert.NotNil(err)
}

// Tests that the server will successfully reload its network connections,
// downgrading from TLS to plaintext if the server's TLS configuration changes.
func TestServer_Reload_TLSConnections_TLSToPlaintext_RPC(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	const (
		cafile  = "../helper/tlsutil/testdata/ca.pem"
		foocert = "../helper/tlsutil/testdata/nomad-foo.pem"
		fookey  = "../helper/tlsutil/testdata/nomad-foo-key.pem"
	)

	dir := tmpDir(t)
	defer os.RemoveAll(dir)
	s1 := testServer(t, func(c *Config) {
		c.DataDir = path.Join(dir, "nodeB")
		c.TLSConfig = &config.TLSConfig{
			EnableHTTP:           true,
			EnableRPC:            true,
			VerifyServerHostname: true,
			CAFile:               cafile,
			CertFile:             foocert,
			KeyFile:              fookey,
		}
	})
	defer s1.Shutdown()

	newTLSConfig := &config.TLSConfig{}

	err := s1.reloadTLSConnections(newTLSConfig)
	assert.Nil(err)
	assert.True(s1.config.TLSConfig.Equals(newTLSConfig))

	time.Sleep(10 * time.Second)

	codec := rpcClient(t, s1)

	node := mock.Node()
	req := &structs.NodeRegisterRequest{
		Node:         node,
		WriteRequest: structs.WriteRequest{Region: "global"},
	}

	var resp structs.GenericResponse
	err = msgpackrpc.CallWithCodec(codec, "Node.Register", req, &resp)
	assert.Nil(err)
}

// Test that Raft connections are reloaded as expected when a Nomad server is
// upgraded from plaintext to TLS
func TestServer_Reload_TLSConnections_Raft(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()
	const (
		cafile  = "../../helper/tlsutil/testdata/ca.pem"
		foocert = "../../helper/tlsutil/testdata/nomad-foo.pem"
		fookey  = "../../helper/tlsutil/testdata/nomad-foo-key.pem"
		barcert = "../dev/tls_cluster/certs/nomad.pem"
		barkey  = "../dev/tls_cluster/certs/nomad-key.pem"
	)
	dir := tmpDir(t)
	defer os.RemoveAll(dir)
	s1 := testServer(t, func(c *Config) {
		c.BootstrapExpect = 2
		c.DevMode = false
		c.DevDisableBootstrap = true
		c.DataDir = path.Join(dir, "node1")
		c.NodeName = "node1"
		c.Region = "regionFoo"
	})
	defer s1.Shutdown()

	s2 := testServer(t, func(c *Config) {
		c.BootstrapExpect = 2
		c.DevMode = false
		c.DevDisableBootstrap = true
		c.DataDir = path.Join(dir, "node2")
		c.NodeName = "node2"
		c.Region = "regionFoo"
	})
	defer s2.Shutdown()

	testJoin(t, s1, s2)

	testutil.WaitForResult(func() (bool, error) {
		peers, _ := s1.numPeers()
		return peers == 2, nil
	}, func(err error) {
		t.Fatalf("should have 2 peers")
	})

	// the server should be connected to the rest of the cluster
	testutil.WaitForLeader(t, s2.RPC)

	{
		// assert that a job register request will succeed
		codec := rpcClient(t, s2)
		job := mock.Job()
		req := &structs.JobRegisterRequest{
			Job: job,
			WriteRequest: structs.WriteRequest{
				Region:    "regionFoo",
				Namespace: job.Namespace,
			},
		}

		// Fetch the response
		var resp structs.JobRegisterResponse
		err := msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp)
		assert.Nil(err)

		// Check for the job in the FSM of each server in the cluster
		{
			state := s2.fsm.State()
			ws := memdb.NewWatchSet()
			out, err := state.JobByID(ws, job.Namespace, job.ID)
			assert.Nil(err)
			assert.NotNil(out)
			assert.Equal(out.CreateIndex, resp.JobModifyIndex)
		}
		{
			state := s1.fsm.State()
			ws := memdb.NewWatchSet()
			out, err := state.JobByID(ws, job.Namespace, job.ID)
			assert.Nil(err)
			assert.NotNil(out) // TODO Occasionally is flaky
			assert.Equal(out.CreateIndex, resp.JobModifyIndex)
		}
	}

	newTLSConfig := &config.TLSConfig{
		EnableHTTP:        true,
		VerifyHTTPSClient: true,
		CAFile:            cafile,
		CertFile:          foocert,
		KeyFile:           fookey,
	}

	err := s1.reloadTLSConnections(newTLSConfig)
	assert.Nil(err)

	{
		// assert that a job register request will fail between servers that
		// should not be able to communicate over Raft
		codec := rpcClient(t, s2)
		job := mock.Job()
		req := &structs.JobRegisterRequest{
			Job: job,
			WriteRequest: structs.WriteRequest{
				Region:    "global",
				Namespace: job.Namespace,
			},
		}

		var resp structs.JobRegisterResponse
		err := msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp)
		assert.NotNil(err)

		// Check that the job was not persisted
		state := s2.fsm.State()
		ws := memdb.NewWatchSet()
		out, err := state.JobByID(ws, job.Namespace, job.ID)
		assert.Nil(out)
	}

	secondNewTLSConfig := &config.TLSConfig{
		EnableHTTP:        true,
		VerifyHTTPSClient: true,
		CAFile:            cafile,
		CertFile:          barcert,
		KeyFile:           barkey,
	}

	// Now, transition the other server to TLS, which should restore their
	// ability to communicate.
	err = s2.reloadTLSConnections(secondNewTLSConfig)
	assert.Nil(err)

	// the server should be connected to the rest of the cluster
	testutil.WaitForLeader(t, s2.RPC)

	{
		// assert that a job register request will succeed
		codec := rpcClient(t, s2)
		job := mock.Job()
		req := &structs.JobRegisterRequest{
			Job: job,
			WriteRequest: structs.WriteRequest{
				Region:    "regionFoo",
				Namespace: job.Namespace,
			},
		}

		// Fetch the response
		var resp structs.JobRegisterResponse
		err := msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp)
		assert.Nil(err)

		// Check for the job in the FSM of each server in the cluster
		{
			state := s2.fsm.State()
			ws := memdb.NewWatchSet()
			out, err := state.JobByID(ws, job.Namespace, job.ID)
			assert.Nil(err)
			assert.NotNil(out)
			assert.Equal(out.CreateIndex, resp.JobModifyIndex)
		}
		{
			state := s1.fsm.State()
			ws := memdb.NewWatchSet()
			out, err := state.JobByID(ws, job.Namespace, job.ID)
			assert.Nil(err)
			assert.NotNil(out)
			assert.Equal(out.CreateIndex, resp.JobModifyIndex)
		}
	}
}
