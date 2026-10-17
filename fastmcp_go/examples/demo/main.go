// Demo is the Go equivalent of the Python FastMCP README example.
//
// Run it over stdio:
//
//	go run ./examples/demo
//
// Or point Claude Desktop / Claude Code at it:
//
//	{"mcpServers": {"demo-go": {"command": "go", "args": ["run", "/path/to/fastmcp_go/examples/demo"]}}}
package main

import (
	"fmt"
	"strings"

	"github.com/PrefectHQ/fastmcp/fastmcp_go/fastmcp"
)

type AddArgs struct {
	A int `json:"a" description:"First number"`
	B int `json:"b" description:"Second number"`
}

// add returns the sum of two integers.
func add(args AddArgs) int {
	return args.A + args.B
}

type GreetArgs struct {
	Name  string `json:"name" description:"Who to greet"`
	Shout bool   `json:"shout,omitempty" description:"Return the greeting in upper case"`
}

func greet(args GreetArgs) string {
	// Printing is safe: while the server runs, stdout goes to stderr so it
	// cannot corrupt the protocol stream. This line shows up in the server log.
	fmt.Println("greet called for", args.Name)

	msg := fmt.Sprintf("Hello, %s!", args.Name)
	if args.Shout {
		msg = strings.ToUpper(msg)
	}
	return msg
}

type WeatherArgs struct {
	City string `json:"city" description:"City name, e.g. London"`
}

type Weather struct {
	City       string `json:"city"`
	TempC      int    `json:"temp_c"`
	Conditions string `json:"conditions"`
}

// Made-up data so the demo needs no API key or network.
var forecasts = map[string]Weather{
	"london": {City: "London", TempC: 14, Conditions: "Cloudy"},
	"paris":  {City: "Paris", TempC: 18, Conditions: "Sunny"},
	"tokyo":  {City: "Tokyo", TempC: 22, Conditions: "Rain"},
}

// weather shows a struct result and an error path.
func weather(args WeatherArgs) (Weather, error) {
	w, ok := forecasts[strings.ToLower(args.City)]
	if !ok {
		return Weather{}, fastmcp.NewToolError("no weather data for %q", args.City)
	}
	return w, nil
}

func main() {
	mcp := fastmcp.New("Demo 🚀")

	mcp.Tool(add, fastmcp.WithDescription("Add two numbers"))
	mcp.Tool(greet, fastmcp.WithDescription("Greet someone by name"))
	mcp.Tool(weather, fastmcp.WithDescription("Get the current weather for a city"))

	mcp.Run()
}
