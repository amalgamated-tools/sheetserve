package server

import (
	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
)

// Option is a functional option for configuring a Server.
type Option func(*Server)

// WithPort sets the TCP port the server listens on.
func WithPort(port int) Option {
	return func(s *Server) {
		s.port = port
	}
}

// WithConfig sets the server's configuration.
func WithConfig(cfg *config.Config) Option {
	return func(s *Server) {
		s.cfg = cfg
	}
}

// WithDB sets the server's database.
func WithDB(d *db.DB) Option {
	return func(s *Server) {
		s.db = d
	}
}
