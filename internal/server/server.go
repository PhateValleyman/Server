package server

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
)

func Main() int {
	programName := os.Args[0]
	errorLog := log.New(os.Stderr, "", log.LstdFlags)
	serveLog := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

	flags := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	flags.Usage = func() {
		out := flags.Output()
		fmt.Fprintf(out, "Usage: %v [dir]\n\n", programName)
		fmt.Fprint(out, "  [dir] is optional; if not passed, '.' is used.\n\n")
		fmt.Fprint(out, "  By default, the server listens on localhost:8080. Both the\n")
		fmt.Fprint(out, "  host and the port are configurable with flags. Set the host\n")
		fmt.Fprint(out, "  to something else if you want the server to listen on a\n")
		fmt.Fprint(out, "  specific network interface. Setting the port to 0 will\n")
		fmt.Fprint(out, "  instruct the server to pick a random available port.\n\n")
		flags.PrintDefaults()
	}

	versionFlag := flags.Bool("version", false, "print version and exit")
	hostFlag := flags.String("host", "localhost", "specific host to listen on")
	portFlag := flags.String("port", "8080", "port to listen on; if 0, a random available port will be used")
	addrFlag := flags.String("addr", "localhost:8080", "full address (host:port) to listen on; don't use this if 'port' or 'host' are set")
	silentFlag := flags.Bool("silent", false, "suppress messages from output (reporting only errors)")
	corsFlag := flags.Bool("cors", false, `enable CORS by setting the Access-Control-Allow-Origin header to "*" in responses`)
	coopFlag := flags.Bool("coop", false, `enable COOP and COEP by setting the Cross-Origin-Opener-Policy header to "same-origin" and the Cross-Origin-Embedder-Policy header to "require-corp" in responses`)
	tlsFlag := flags.Bool("tls", false, "enable HTTPS serving with TLS")
	certFlag := flags.String("certfile", "cert.pem", "TLS certificate file to use with -tls")
	keyFlag := flags.String("keyfile", "key.pem", "TLS key file to use with -tls")

	flags.Parse(os.Args[1:])

	if *versionFlag {
		if buildInfo, ok := debug.ReadBuildInfo(); ok {
			fmt.Printf("%v %v\n", programName, buildInfo.Main.Version)
		} else {
			errorLog.Printf("version info unavailable! run 'go version -m %v'", programName)
		}
		os.Exit(0)
	}

	if *silentFlag {
		serveLog.SetOutput(io.Discard)
	}

	if len(flags.Args()) > 1 {
		errorLog.Println("Error: too many command-line arguments")
		flags.Usage()
		os.Exit(1)
	}

	rootDir := "."
	if len(flags.Args()) == 1 {
		rootDir = flags.Args()[0]
	}

	allSetFlags := flagsSet(flags)
	if allSetFlags["addr"] && (allSetFlags["host"] || allSetFlags["port"]) {
		errorLog.Println("Error: if -addr is set, -host and -port must remain unset")
		flags.Usage()
		os.Exit(1)
	}

	var addr string
	if allSetFlags["addr"] {
		addr = *addrFlag
	} else {
		addr = *hostFlag + ":" + *portFlag
	}

	srv := &http.Server{
		Addr: addr,
		TLSConfig: &tls.Config{
			MinVersion:               tls.VersionTLS13,
			PreferServerCipherSuites: true,
		},
	}

	shutdownCh := make(chan struct{})
	go func() {
		<-shutdownCh
		srv.Shutdown(context.Background())
	}()

	testingKey := os.Getenv("TESTING_KEY")

	mux := http.NewServeMux()
	mux.HandleFunc("/__internal/__shutdown", func(w http.ResponseWriter, r *http.Request) {
		if testingKey != "" && r.Header.Get("Static-Server-Testing-Key") == testingKey {
			w.WriteHeader(http.StatusOK)
			defer close(shutdownCh)
		} else {
			http.Error(w, "403 Forbidden", http.StatusForbidden)
		}
	})

	fileHandler := serveLogger(serveLog, http.FileServer(http.Dir(rootDir)))
	if *corsFlag {
		fileHandler = enableCORS(fileHandler)
	}
	if *coopFlag {
		fileHandler = enableCOOP(fileHandler)
	}
	mux.Handle("/", fileHandler)
	mux.HandleFunc("/upload", uploadHandler(rootDir))

	srv.Handler = mux

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		errorLog.Println(err)
		return 1
	}

	scheme := "http://"
	if *tlsFlag {
		scheme = "https://"
	}

	serveLog.Printf("Serving directory %q on %v%v", rootDir, scheme, listener.Addr())

	if *tlsFlag {
		err = srv.ServeTLS(listener, *certFlag, *keyFlag)
	} else {
		err = srv.Serve(listener)
	}

	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		errorLog.Println("Error in Serve:", err)
		return 1
	} else {
		return 0
	}
}

func flagsSet(flags *flag.FlagSet) map[string]bool {
	s := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) {
		s[f.Name] = true
	})
	return s
}

func serveLogger(logger *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteHost, _, _ := strings.Cut(r.RemoteAddr, ":")
		logger.Printf("%v %v %v\n", remoteHost, r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func enableCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}

func enableCOOP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
		next.ServeHTTP(w, r)
	})
}

// uploadHandler handles file uploads.  This is a VERY basic example and needs improvements.
func uploadHandler(uploadDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		err := r.ParseMultipartForm(32 << 20)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		file, handler, err := r.FormFile("uploadfile")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()

		dst, err := os.Create(uploadDir + "/" + handler.Filename)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer dst.Close()

		_, err = io.Copy(dst, file)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		fmt.Fprintf(w, "File uploaded successfully: %s\n", handler.Filename)
	}
}
