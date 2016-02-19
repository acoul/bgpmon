package main

import (
	"errors"
	"flag"
	"fmt"
	"net"

	"github.com/hamersaw/bgpmon/log"
	"github.com/hamersaw/bgpmon/module"
	"github.com/hamersaw/bgpmon/module/bgp"
	"github.com/hamersaw/bgpmon/module/gobgp"
	pb "github.com/hamersaw/bgpmon/proto/bgpmond"
	"github.com/hamersaw/bgpmon/session"

	"github.com/BurntSushi/toml"
	"github.com/google/uuid"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

var configFile string
var config BgpmondConfig

type BgpmondConfig struct {
	Address string
	DebugOut string
	ErrorOut string
}

func init() {
	flag.StringVar(&configFile, "config_file", "", "bgpmond toml configuration file")
}

func main() {
	flag.Parse()

	if _, err := toml.DecodeFile(configFile, &config); err != nil {
		panic(err)
	}

	debugClose, errorClose, err := log.Init(config.DebugOut, config.ErrorOut)
	if err != nil {
		panic(err)
	}
	defer debugClose()
	defer errorClose()

	listen, err := net.Listen("tcp", config.Address)
	if err != nil {
		panic(err)
	}

	bgpmondServer := Server {
		sessions: make(map[string]session.Session),
		modules: make(map[string]module.Moduler),
	}

	grpcServer := grpc.NewServer()
	pb.RegisterBgpmondServer(grpcServer, bgpmondServer)
	grpcServer.Serve(listen)
}

type Server struct {
	sessions map[string]session.Session //map from uuid to session interface
	modules  map[string]module.Moduler  //map from uuid to running module interface
}

/*
 * Module RPC Calls
 */
func (s Server) StartModule(ctx context.Context, config *pb.StartModuleConfig) (*pb.StartModuleResult, error) {
	result := new(pb.StartModuleResult)
	var mod module.Moduler
	var err error

	switch config.Type {
	case pb.StartModuleConfig_GOBGP_LINK:
		goBGPLinkConfig := config.GetGobgpLinkModule()
		mod, err = gobgp.NewGoBGPLinkModule(goBGPLinkConfig.Address, session.IOSessions { nil, nil })
	case pb.StartModuleConfig_PREFIX_HIJACK:
		prefixHijackConfig := config.GetPrefixHijackModule()
		mod, err = bgp.NewPrefixHijackModule(prefixHijackConfig.Prefix, session.IOSessions { nil, nil })
	default:
		result.Success = false
		result.ErrorMessage = "unimplemented module type"
		return result, nil
	}

	if err == nil {
		moduleID := newID()
		s.modules[moduleID] = mod

		result.Success = true
		result.ModuleId = moduleID
	} else {
		result.Success = false
		result.ErrorMessage = fmt.Sprintf("%v", err)
	}

	return result, nil
}

func (s Server) StopModule(ctx context.Context, config *pb.StopModuleConfig) (*pb.StopModuleResult, error) {
	return nil, nil
}

/*
 * Session RPC Calls
 */
func (s Server) CloseSession(ctx context.Context, config *pb.CloseSessionConfig) (result *pb.CloseSessionResult, err error) {
	err = errors.New("unimplemented")
	return
}

func (s Server) OpenSession(ctx context.Context, config *pb.OpenSessionConfig) (*pb.OpenSessionResult, error) {
	result := new(pb.OpenSessionResult)
	var sess session.Session
	var err error

	switch config.Type {
	case pb.OpenSessionConfig_CASSANDRA:
		casConfig := config.GetCassandraSession()
		sess, err = session.NewCassandraSession(casConfig.Username, casConfig.Password, casConfig.Hosts)
	case pb.OpenSessionConfig_FILE:
		fileConfig := config.GetFileSession()
		sess, err = session.NewFileSession(fileConfig.Filename)
	default:
		result.Success = false;
		result.ErrorMessage = "unimplemented session type"
		return result, nil
	}

	if err == nil {
		sessionID := newID()
		s.sessions[sessionID] = sess

		result.Success = true
		result.SessionId = sessionID
	} else {
		result.Success = false
		result.ErrorMessage = fmt.Sprintf("*v", err)
	}

	return result, nil
}

func newID() string {
	return uuid.New()
}
