package server

// Option is a functional option for configuring a Server.
type Option func(*Server)

// WithPort sets the TCP port the server listens on.
func WithPort(port int) Option {
	return func(s *Server) {
		s.port = port
	}
}
