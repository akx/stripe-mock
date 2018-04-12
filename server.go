package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lestrrat/go-jsval"
	"github.com/stripe/stripe-mock/param/coercer"
	"github.com/stripe/stripe-mock/param/parser"
	"github.com/stripe/stripe-mock/spec"
)

//
// Public types
//

type ErrorInfo struct {
}

// ExpansionLevel represents expansions on a single "level" of resource. It may
// have subexpansions that are meant to take effect on resources that are
// nested below it (on other levels).
type ExpansionLevel struct {
	expansions map[string]*ExpansionLevel

	// wildcard specifies that everything should be expanded.
	wildcard bool
}

type ResponseError struct {
	ErrorInfo struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// StubServer handles incoming HTTP requests and responds to them appropriately
// based off the set of OpenAPI routes that it's been configured with.
type StubServer struct {
	fixtures *spec.Fixtures
	routes   map[spec.HTTPVerb][]stubServerRoute
	spec     *spec.Spec
}

// HandleRequest handes an HTTP request directed at the API stub.
func (s *StubServer) HandleRequest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	fmt.Printf("Request: %v %v\n", r.Method, r.URL.Path)

	auth := r.Header.Get("Authorization")
	if !validateAuth(auth) {
		message := fmt.Sprintf(invalidAuthorization, auth)
		stripeError := createStripeError(typeInvalidRequestError, message)
		writeResponse(w, r, start, http.StatusUnauthorized, stripeError)
		return
	}

	// Every response needs a Request-Id header except the invalid authorization
	w.Header().Set("Request-Id", "req_123")

	route, id := s.routeRequest(r)
	if route == nil {
		message := fmt.Sprintf(invalidRoute, r.Method, r.URL.Path)
		stripeError := createStripeError(typeInvalidRequestError, message)
		writeResponse(w, r, start, http.StatusNotFound, stripeError)
		return
	}

	response, ok := route.operation.Responses["200"]
	if !ok {
		fmt.Printf("Couldn't find 200 response in spec\n")
		writeResponse(w, r, start, http.StatusInternalServerError,
			createInternalServerError())
		return
	}
	responseContent, ok := response.Content["application/json"]
	if !ok || responseContent.Schema == nil {
		fmt.Printf("Couldn't find application/json in response\n")
		writeResponse(w, r, start, http.StatusInternalServerError,
			createInternalServerError())
		return
	}

	if verbose {
		fmt.Printf("ID extracted from route: %+v\n", id)
		fmt.Printf("Response schema: %s\n", responseContent.Schema)
	}

	var formString string
	if r.Method == "GET" {
		formString = r.URL.RawQuery
	} else {
		formBytes, err := ioutil.ReadAll(r.Body)
		if err != nil {
			fmt.Printf("Couldn't read request body: %v\n", err)
			writeResponse(w, r, start, http.StatusInternalServerError,
				createInternalServerError())
			return
		}
		r.Body.Close()
		formString = string(formBytes)
	}
	requestData, err := parser.ParseFormString(formString)
	if err != nil {
		fmt.Printf("Couldn't parse query/body: %v\n", err)
		writeResponse(w, r, start, http.StatusInternalServerError,
			createInternalServerError())
		return
	}

	if verbose {
		if formString != "" {
			fmt.Printf("Request data: %s\n", formString)
		} else {
			fmt.Printf("Request data: (none)\n")
		}
	}

	// Currently we only validate parameters in the request body, but we should
	// really validate query and URL parameters as well now that we've
	// transitioned to OpenAPI 3.0
	bodySchema := getRequestBodySchema(route.operation)
	if bodySchema != nil {
		err := coercer.CoerceParams(bodySchema, requestData)
		if err != nil {
			fmt.Printf("Coercion error: %v\n", err)
			message := fmt.Sprintf("Request error: %v", err)
			stripeError := createStripeError(typeInvalidRequestError, message)
			writeResponse(w, r, start, http.StatusBadRequest, stripeError)
			return
		}

		err = route.requestBodyValidator.Validate(requestData)
		if err != nil {
			fmt.Printf("Validation error: %v\n", err)
			message := fmt.Sprintf("Request error: %v", err)
			stripeError := createStripeError(typeInvalidRequestError, message)
			writeResponse(w, r, start, http.StatusBadRequest, stripeError)
			return
		}
	}

	expansions, rawExpansions := extractExpansions(requestData)
	if verbose {
		fmt.Printf("Expansions: %+v\n", rawExpansions)
	}

	generator := DataGenerator{s.spec.Components.Schemas, s.fixtures}
	responseData, err := generator.Generate(
		responseContent.Schema,
		r.URL.Path,
		id,
		expansions)
	if err != nil {
		fmt.Printf("Couldn't generate response: %v\n", err)
		writeResponse(w, r, start, http.StatusInternalServerError,
			createInternalServerError())
		return
	}
	if verbose {
		responseDataJson, err := json.MarshalIndent(responseData, "", "  ")
		if err != nil {
			panic(err)
		}
		fmt.Printf("Response data: %s\n", responseDataJson)
	}
	writeResponse(w, r, start, http.StatusOK, responseData)
}

func (s *StubServer) initializeRouter() error {
	var numEndpoints int
	var numPaths int
	var numValidators int

	s.routes = make(map[spec.HTTPVerb][]stubServerRoute)

	componentsForValidation := spec.GetComponentsForValidation(&s.spec.Components)

	for path, verbs := range s.spec.Paths {
		numPaths++

		pathPattern := compilePath(path)

		if verbose {
			fmt.Printf("Compiled path: %v\n", pathPattern.String())
		}

		for verb, operation := range verbs {
			numEndpoints++

			requestBodySchema := getRequestBodySchema(operation)
			var requestBodyValidator *jsval.JSVal
			if requestBodySchema != nil {
				var err error
				requestBodyValidator, err = spec.GetValidatorForOpenAPI3Schema(
					requestBodySchema, componentsForValidation)
				if err != nil {
					return err
				}
			}

			// Note that this may be nil if no suitable validator could be
			// generated.
			if requestBodyValidator != nil {
				numValidators++
			}

			// We use whether the route ends with a parameter as a heuristic as
			// to whether we should expect an object's primary ID in the URL.
			var endsWithID bool
			for _, suffix := range endsWithIDSuffixes {
				if strings.HasSuffix(string(path), suffix) {
					endsWithID = true
					break
				}
			}

			route := stubServerRoute{
				endsWithID:           endsWithID,
				pattern:              pathPattern,
				operation:            operation,
				requestBodyValidator: requestBodyValidator,
			}

			// net/http will always give us verbs in uppercase, so build our
			// routing table this way too
			verb = spec.HTTPVerb(strings.ToUpper(string(verb)))

			s.routes[verb] = append(s.routes[verb], route)
		}
	}

	fmt.Printf("Routing to %v path(s) and %v endpoint(s) with %v validator(s)\n",
		numPaths, numEndpoints, numValidators)
	return nil
}

// routeRequest tries to find a matching route for the given request. If
// successful, it returns the matched route and where possible, an extracted ID
// which comes from the last capture group in the URL. An ID is only returned
// if it looks like it's supposed to be the primary identifier of the returned
// object (i.e., the route's pattern ended with a parameter). A nil is returned
// as the second return value when no primary ID is available.
func (s *StubServer) routeRequest(r *http.Request) (*stubServerRoute, *string) {
	verbRoutes := s.routes[spec.HTTPVerb(r.Method)]
	for _, route := range verbRoutes {
		matches := route.pattern.FindAllStringSubmatch(r.URL.Path, -1)

		if len(matches) < 1 {
			continue
		}

		// There will only ever be a single match in the string (this match
		// contains the entire match plus all capture groups).
		firstMatch := matches[0]

		// This route doesn't appear to contain the ID of the primary object
		// being returned. Return the route only.
		if !route.endsWithID {
			return &route, nil
		}

		// Return the route along with the likely ID.
		return &route, &firstMatch[len(firstMatch)-1]
	}
	return nil, nil
}

//
// Private values
//

const (
	invalidAuthorization = "Please authenticate by specifying an " +
		"`Authorization` header with any valid looking testmode secret API " +
		"key. For example, `Authorization: Bearer sk_test_123`. " +
		"Authorization was '%s'."

	invalidRoute = "Unrecognized request URL (%s: %s)."

	internalServerError = "An internal error occurred."

	typeInvalidRequestError = "invalid_request_error"
)

// Suffixes for which we will try to exact an object's ID from the path.
var endsWithIDSuffixes = [...]string{
	// The general case: we're looking for the end of an OpenAPI URL parameter.
	"}",

	// These are resource "actions". They don't take the standard form, but we
	// can expect an object's primary ID to live right before them in a path.
	"/close",
	"/pay",
}

var pathParameterPattern = regexp.MustCompile(`\{(\w+)\}`)

//
// Private types
//

// stubServerRoute is a single route in a StubServer's routing table. It has a
// pattern to match an incoming path and a description of the method that would
// be executed in the event of a match.
type stubServerRoute struct {
	endsWithID           bool
	pattern              *regexp.Regexp
	operation            *spec.Operation
	requestBodyValidator *jsval.JSVal
}

//
// Private functions
//

func compilePath(path spec.Path) *regexp.Regexp {
	pattern := `\A`
	parts := strings.Split(string(path), "/")

	for _, part := range parts {
		if part == "" {
			continue
		}

		submatches := pathParameterPattern.FindAllStringSubmatch(part, -1)
		if submatches == nil {
			pattern += `/` + part
		} else {
			pattern += `/(?P<` + submatches[0][1] + `>[\w-_.]+)`
		}
	}

	return regexp.MustCompile(pattern + `\z`)
}

// Helper to create an internal server error for API issues.
func createInternalServerError() *ResponseError {
	return createStripeError(typeInvalidRequestError, internalServerError)
}

// This creates a Stripe error to return in case of API errors.
func createStripeError(errorType string, errorMessage string) *ResponseError {
	return &ResponseError{
		ErrorInfo: struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		}{
			Message: errorMessage,
			Type:    errorType,
		},
	}
}

func extractExpansions(data map[string]interface{}) (*ExpansionLevel, []string) {
	expand, ok := data["expand"]
	if !ok {
		return nil, nil
	}

	var expansions []string

	expandStr, ok := expand.(string)
	if ok {
		expansions = append(expansions, expandStr)
		return parseExpansionLevel(expansions), expansions
	}

	expandArr, ok := expand.([]interface{})
	if ok {
		for _, expand := range expandArr {
			expandStr := expand.(string)
			expansions = append(expansions, expandStr)
		}
		return parseExpansionLevel(expansions), expansions
	}

	return nil, nil
}

func getRequestBodySchema(operation *spec.Operation) *spec.Schema {
	if operation.RequestBody == nil {
		return nil
	}
	mediaType, mediaTypePresent :=
		operation.RequestBody.Content["application/x-www-form-urlencoded"]
	if !mediaTypePresent {
		return nil
	}
	return mediaType.Schema
}

func isCurl(userAgent string) bool {
	return strings.HasPrefix(userAgent, "curl/")
}

// parseExpansionLevel parses a set of raw expansions from a request query
// string or form and produces a structure more useful for performing actual
// expansions.
func parseExpansionLevel(raw []string) *ExpansionLevel {
	sort.Strings(raw)

	level := &ExpansionLevel{expansions: make(map[string]*ExpansionLevel)}
	groups := make(map[string][]string)

	for _, expansion := range raw {
		parts := strings.Split(expansion, ".")
		if len(parts) == 1 {
			if parts[0] == "*" {
				level.wildcard = true
			} else {
				level.expansions[parts[0]] =
					&ExpansionLevel{expansions: make(map[string]*ExpansionLevel)}
			}
		} else {
			groups[parts[0]] = append(groups[parts[0]], strings.Join(parts[1:], "."))
		}
	}

	for key, subexpansions := range groups {
		level.expansions[key] = parseExpansionLevel(subexpansions)
	}

	return level
}

func validateAuth(auth string) bool {
	if auth == "" {
		return false
	}

	parts := strings.Split(auth, " ")

	// Expect ["Bearer", "sk_test_123"] or ["Basic", "aaaaa"]
	if len(parts) != 2 || parts[1] == "" {
		return false
	}

	var key string
	switch parts[0] {
	case "Basic":
		keyBytes, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return false
		}
		key = string(keyBytes)

	case "Bearer":
		key = parts[1]

	default:
		return false
	}

	keyParts := strings.Split(key, "_")

	// Expect ["sk", "test", "123"]
	if len(keyParts) != 3 {
		return false
	}

	if keyParts[0] != "sk" {
		return false
	}

	if keyParts[1] != "test" {
		return false
	}

	// Expect something (anything but an empty string) in the third position
	if len(keyParts[2]) == 0 {
		return false
	}

	return true
}

func writeResponse(w http.ResponseWriter, r *http.Request, start time.Time, status int, data interface{}) {
	if data == nil {
		data = http.StatusText(status)
	}

	var encodedData []byte
	var err error

	if !isCurl(r.Header.Get("User-Agent")) {
		encodedData, err = json.Marshal(&data)
	} else {
		encodedData, err = json.MarshalIndent(&data, "", "  ")
		encodedData = append(encodedData, '\n')
	}

	if err != nil {
		fmt.Printf("Error serializing response: %v\n", err)
		writeResponse(w, r, start, http.StatusInternalServerError, nil)
		return
	}

	w.Header().Set("Stripe-Mock-Version", version)

	w.WriteHeader(status)
	_, err = w.Write(encodedData)
	if err != nil {
		fmt.Printf("Error writing to client: %v\n", err)
	}
	fmt.Printf("Response: elapsed=%v status=%v\n", time.Now().Sub(start), status)
}
