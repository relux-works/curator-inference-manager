package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	if duplicateModelIDArgument(args) {
		log.Fatal("duplicate --model-id is refused")
	}
	model, alias, host, port, forwarded := parseArguments(args)
	if argvPath := os.Getenv("CATALOG_FAKE_ARGV_FILE"); argvPath != "" {
		if err := os.WriteFile(argvPath, []byte(strings.Join(os.Args[1:], "\n")+"\n"), 0o600); err != nil {
			log.Fatal(err)
		}
	}
	if pidPath := os.Getenv("CATALOG_FAKE_PID_FILE"); pidPath != "" {
		if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			log.Fatal(err)
		}
	}
	model = reportedModelID(os.Getenv("CATALOG_FAKE_SERVED_MODEL_ID"), model, alias)
	modelIDs := []string{model}
	if override, ok := os.LookupEnv("CATALOG_FAKE_MODEL_IDS"); ok {
		modelIDs = nil
		for _, id := range strings.Split(override, ",") {
			if id != "" {
				modelIDs = append(modelIDs, id)
			}
		}
	}
	for _, additional := range strings.Split(os.Getenv("CATALOG_FAKE_ADDITIONAL_MODEL_IDS"), ",") {
		if additional != "" {
			modelIDs = append(modelIDs, additional)
		}
	}
	if model == "" {
		log.Fatal("a served model id or CATALOG_FAKE_SERVED_MODEL_ID is required")
	}
	if forwarded != "" {
		var portText string
		host, portText, _ = net.SplitHostPort(forwarded)
		port, _ = strconv.Atoi(portText)
	}
	if host == "" || port == 0 {
		log.Fatal("host and port are required")
	}
	mux := http.NewServeMux()
	models := func(writer http.ResponseWriter, _ *http.Request) {
		if requestPath := os.Getenv("CATALOG_FAKE_MODELS_REQUEST_FILE"); requestPath != "" {
			if err := os.WriteFile(requestPath, []byte("requested\n"), 0o600); err != nil {
				log.Fatal(err)
			}
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"object": "list",
			"data":   catalogModelRows(modelIDs),
		})
	}
	mux.HandleFunc("/v1/models", models)
	mux.HandleFunc("/models", models)
	mux.HandleFunc("/agents-infra/resources", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"schema":    "agents-infra.pi.shared-runtime.resource-observation.v1",
			"model":     model,
			"inference": map[string]string{"state": "idle"},
		})
	})
	address := net.JoinHostPort(host, strconv.Itoa(port))
	if err := http.ListenAndServe(address, mux); err != nil {
		log.Fatal(err)
	}
}

func duplicateModelIDArgument(args []string) bool {
	count := 0
	for index := 0; index < len(args); index++ {
		if args[index] == "--model-id" {
			count++
			if index+1 < len(args) {
				index++
			}
		} else if strings.HasPrefix(args[index], "--model-id=") {
			count++
		}
	}
	return count > 1
}

func reportedModelID(override, model, alias string) string {
	if override != "" {
		return override
	}
	if alias != "" {
		return strings.SplitN(alias, ",", 2)[0]
	}
	if model != "" {
		return path.Base(model)
	}
	return ""
}

func parseArguments(args []string) (model, alias, host string, port int, forwarded string) {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		value := ""
		if strings.Contains(argument, "=") {
			parts := strings.SplitN(argument, "=", 2)
			argument, value = parts[0], parts[1]
		} else if isCatalogValueFlag(argument) && index+1 < len(args) {
			value = args[index+1]
			index++
		} else {
			continue
		}
		switch argument {
		case "--model", "-m":
			model = value
		case "--alias", "-a", "--model-id":
			alias = value
		case "--host":
			host = value
		case "--port":
			port, _ = strconv.Atoi(value)
		case "-L":
			parts := strings.Split(value, ":")
			if len(parts) >= 4 {
				forwarded = net.JoinHostPort(parts[0], parts[1])
			}
		}
	}
	return model, alias, host, port, forwarded
}

func isCatalogValueFlag(argument string) bool {
	switch argument {
	case "--model", "-m", "--alias", "-a", "--model-id", "--host", "--port", "-L":
		return true
	default:
		return false
	}
}

func catalogModelRows(ids []string) []map[string]string {
	rows := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, map[string]string{"id": id})
	}
	return rows
}
