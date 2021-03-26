package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"

	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/node"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/rbac"
	"github.com/lxc/lxd/lxd/response"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/logger"
)

var operationCmd = APIEndpoint{
	Path: "operations/{id}",

	Delete: APIEndpointAction{Handler: operationDelete, AccessHandler: allowAuthenticated},
	Get:    APIEndpointAction{Handler: operationGet, AccessHandler: allowAuthenticated},
}

var operationsCmd = APIEndpoint{
	Path: "operations",

	Get: APIEndpointAction{Handler: operationsGet, AccessHandler: allowAuthenticated},
}

var operationWait = APIEndpoint{
	Path: "operations/{id}/wait",

	Get: APIEndpointAction{Handler: operationWaitGet, AllowUntrusted: true},
}

var operationWebsocket = APIEndpoint{
	Path: "operations/{id}/websocket",

	Get: APIEndpointAction{Handler: operationWebsocketGet, AllowUntrusted: true},
}

// waitForOperations waits for operations to finish. There's a timeout for console/exec operations
// that when reached will shut down the instances forcefully.
// It also watches the cancel channel, and will return if it receives data.
func waitForOperations(s *state.State, chCancel chan struct{}) {
	timeout := time.After(5 * time.Minute)
	tick := time.Tick(time.Second)
	logTick := time.Tick(time.Minute)

	for {
		<-tick

		// Get all the operations
		ops := operations.Clone()

		runningOps := 0

		for _, op := range ops {
			if op.Status() == api.Running {
				runningOps++
			}
		}

		// No more running operations left. Exit function.
		if runningOps == 0 {
			logger.Info("All running operations finished, shutting down")
			return
		}

		execConsoleOps := 0

		for _, op := range ops {
			opType := op.Type()
			if opType == db.OperationCommandExec || opType == db.OperationConsoleShow {
				execConsoleOps++
			}

			_, opAPI, _ := op.Render()
			if opAPI.MayCancel {
				op.Cancel()
			}
		}

		select {
		case <-timeout:
			// We wait up to 5 minutes for exec/console operations to finish.
			// If there are still running operations, we shut down the instances
			// which will terminate the operations.
			if execConsoleOps > 0 {
				logger.Info("Timeout reached, continuing with shutdown")
			}
			return
		case <-logTick:
			// Print log message every minute.
			logger.Infof("Waiting for %d operation(s) to finish", runningOps)
		case <-chCancel:
			// Return here, and ignore any running operations.
			logger.Info("Forcing shutdown, ignoring running operations")
			return
		default:
		}
	}
}

// API functions

// swagger:operation GET /1.0/operations/{id} operations operation_get
//
// Get the operation state
//
// Gets the operation state.
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     description: Operation
//     schema:
//       type: object
//       description: Sync response
//       properties:
//         type:
//           type: string
//           description: Response type
//           example: sync
//         status:
//           type: string
//           description: Status description
//           example: Success
//         status_code:
//           type: int
//           description: Status code
//           example: 200
//         metadata:
//           $ref: "#/definitions/Operation"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func operationGet(d *Daemon, r *http.Request) response.Response {
	id := mux.Vars(r)["id"]

	var body *api.Operation

	// First check if the query is for a local operation from this node
	op, err := operations.OperationGetInternal(id)
	if err == nil {
		_, body, err = op.Render()
		if err != nil {
			return response.SmartError(err)
		}

		return response.SyncResponse(true, body)
	}

	// Then check if the query is from an operation on another node, and, if so, forward it
	var address string
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		operation, err := tx.GetOperationByUUID(id)
		if err != nil {
			return err
		}

		address = operation.NodeAddress
		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	cert := d.endpoints.NetworkCert()
	client, err := cluster.Connect(address, cert, false)
	if err != nil {
		return response.SmartError(err)
	}

	return response.ForwardedResponse(client, r)
}

// swagger:operation DELETE /1.0/operations/{id} operations operation_delete
//
// Cancel the operation
//
// Cancels the operation if supported.
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func operationDelete(d *Daemon, r *http.Request) response.Response {
	id := mux.Vars(r)["id"]

	// First check if the query is for a local operation from this node
	op, err := operations.OperationGetInternal(id)
	if err == nil {
		if op.Permission() != "" {
			projectName := op.Project()
			if projectName == "" {
				projectName = project.Default
			}

			if !rbac.UserHasPermission(r, projectName, op.Permission()) {
				return response.Forbidden(nil)
			}
		}

		_, err = op.Cancel()
		if err != nil {
			return response.BadRequest(err)
		}

		return response.EmptySyncResponse
	}

	// Then check if the query is from an operation on another node, and, if so, forward it
	var address string
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		operation, err := tx.GetOperationByUUID(id)
		if err != nil {
			return err
		}

		address = operation.NodeAddress
		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	cert := d.endpoints.NetworkCert()
	client, err := cluster.Connect(address, cert, false)
	if err != nil {
		return response.SmartError(err)
	}

	return response.ForwardedResponse(client, r)
}

// swagger:operation GET /1.0/operations operations operations_get
//
// Get the operations
//
// Returns a dict of operation type to operation list (URLs).
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     description: API endpoints
//     schema:
//       type: object
//       description: Sync response
//       properties:
//         type:
//           type: string
//           description: Response type
//           example: sync
//         status:
//           type: string
//           description: Status description
//           example: Success
//         status_code:
//           type: int
//           description: Status code
//           example: 200
//         metadata:
//           type: object
//           additionalProperties:
//             type: array
//             items:
//               type: string
//           description: Dict of operation types to operation URLs
//           example: |-
//             {
//               "running": [
//                 "/1.0/operations/6916c8a6-9b7d-4abd-90b3-aedfec7ec7da"
//               ]
//             }
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"

// swagger:operation GET /1.0/operations?recursion=1 operations operations_get_recursion1
//
// Get the operations
//
// Returns a list of operations (structs).
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     description: API endpoints
//     schema:
//       type: object
//       description: Sync response
//       properties:
//         type:
//           type: string
//           description: Response type
//           example: sync
//         status:
//           type: string
//           description: Status description
//           example: Success
//         status_code:
//           type: int
//           description: Status code
//           example: 200
//         metadata:
//           type: array
//           description: List of operations
//           items:
//             $ref: "#/definitions/Operation"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func operationsGet(d *Daemon, r *http.Request) response.Response {
	projectName := projectParam(r)
	recursion := util.IsRecursionRequest(r)

	localOperationURLs := func() (shared.Jmap, error) {
		// Get all the operations
		localOps := operations.Clone()

		// Build a list of URLs
		body := shared.Jmap{}

		for _, v := range localOps {
			if v.Project() != "" && v.Project() != projectName {
				continue
			}
			status := strings.ToLower(v.Status().String())
			_, ok := body[status]
			if !ok {
				body[status] = make([]string, 0)
			}

			body[status] = append(body[status].([]string), v.URL())
		}

		return body, nil
	}

	localOperations := func() (shared.Jmap, error) {
		// Get all the operations
		localOps := operations.Clone()

		// Build a list of operations
		body := shared.Jmap{}

		for _, v := range localOps {
			if v.Project() != "" && v.Project() != projectName {
				continue
			}
			status := strings.ToLower(v.Status().String())
			_, ok := body[status]
			if !ok {
				body[status] = make([]*api.Operation, 0)
			}

			_, op, err := v.Render()
			if err != nil {
				return nil, err
			}

			body[status] = append(body[status].([]*api.Operation), op)
		}

		return body, nil
	}

	// Check if called from a cluster node
	if isClusterNotification(r) {
		// Only return the local data
		if recursion {
			// Recursive queries
			body, err := localOperations()
			if err != nil {
				return response.InternalError(err)
			}

			return response.SyncResponse(true, body)
		}

		// Normal queries
		body, err := localOperationURLs()
		if err != nil {
			return response.InternalError(err)
		}

		return response.SyncResponse(true, body)
	}

	// Start with local operations
	var md shared.Jmap
	var err error

	if recursion {
		md, err = localOperations()
		if err != nil {
			return response.InternalError(err)
		}
	} else {
		md, err = localOperationURLs()
		if err != nil {
			return response.InternalError(err)
		}
	}

	// Check if clustered
	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.InternalError(err)
	}

	// Return now if not clustered
	if !clustered {
		return response.SyncResponse(true, md)
	}

	// Get all nodes with running operations in this project.
	var nodes []string
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error

		nodes, err = tx.GetNodesWithRunningOperations(projectName)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Get local address
	localAddress, err := node.HTTPSAddress(d.db)
	if err != nil {
		return response.InternalError(err)
	}

	cert := d.endpoints.NetworkCert()
	for _, node := range nodes {
		if node == localAddress {
			continue
		}

		// Connect to the remote server
		client, err := cluster.Connect(node, cert, true)
		if err != nil {
			return response.SmartError(err)
		}

		// Get operation data
		ops, err := client.GetOperations()
		if err != nil {
			return response.SmartError(err)
		}

		// Merge with existing data
		for _, op := range ops {
			status := strings.ToLower(op.Status)

			_, ok := md[status]
			if !ok {
				if recursion {
					md[status] = make([]*api.Operation, 0)
				} else {
					md[status] = make([]string, 0)
				}
			}

			if recursion {
				md[status] = append(md[status].([]*api.Operation), &op)
			} else {
				md[status] = append(md[status].([]string), fmt.Sprintf("/1.0/operations/%s", op.ID))
			}
		}
	}

	return response.SyncResponse(true, md)
}

// swagger:operation GET /1.0/operations/{id}/wait?public operations operation_wait_get_untrusted
//
// Wait for the operation
//
// Waits for the operation to reach a final state (or timeout) and retrieve its final state.
//
// When accessed by an untrusted user, the secret token must be provided.
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: secret
//     description: Authentication token
//     type: string
//     example: random-string
//   - in: query
//     name: timeout
//     description: Timeout in seconds (-1 means never)
//     type: int
//     example: -1
// responses:
//   "200":
//     description: Operation
//     schema:
//       type: object
//       description: Sync response
//       properties:
//         type:
//           type: string
//           description: Response type
//           example: sync
//         status:
//           type: string
//           description: Status description
//           example: Success
//         status_code:
//           type: int
//           description: Status code
//           example: 200
//         metadata:
//           $ref: "#/definitions/Operation"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"

// swagger:operation GET /1.0/operations/{id}/wait operations operation_wait_get
//
// Wait for the operation
//
// Waits for the operation to reach a final state (or timeout) and retrieve its final state.
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: timeout
//     description: Timeout in seconds (-1 means never)
//     type: int
//     example: -1
// responses:
//   "200":
//     description: Operation
//     schema:
//       type: object
//       description: Sync response
//       properties:
//         type:
//           type: string
//           description: Response type
//           example: sync
//         status:
//           type: string
//           description: Status description
//           example: Success
//         status_code:
//           type: int
//           description: Status code
//           example: 200
//         metadata:
//           $ref: "#/definitions/Operation"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func operationWaitGet(d *Daemon, r *http.Request) response.Response {
	id := mux.Vars(r)["id"]
	secret := r.FormValue("secret")

	trusted, _, _, _ := d.Authenticate(nil, r)
	if !trusted && secret == "" {
		return response.Forbidden(nil)
	}

	timeout, err := shared.AtoiEmptyDefault(r.FormValue("timeout"), -1)
	if err != nil {
		return response.InternalError(err)
	}

	// First check if the query is for a local operation from this node
	op, err := operations.OperationGetInternal(id)
	if err == nil {
		if secret != "" && op.Metadata()["secret"] != secret {
			return response.Forbidden(nil)
		}

		_, err = op.WaitFinal(timeout)
		if err != nil {
			return response.InternalError(err)
		}

		_, body, err := op.Render()
		if err != nil {
			return response.SmartError(err)
		}

		return response.SyncResponse(true, body)
	}

	// Then check if the query is from an operation on another node, and, if so, forward it
	var address string
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		operation, err := tx.GetOperationByUUID(id)
		if err != nil {
			return err
		}

		address = operation.NodeAddress
		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	cert := d.endpoints.NetworkCert()
	client, err := cluster.Connect(address, cert, false)
	if err != nil {
		return response.SmartError(err)
	}

	return response.ForwardedResponse(client, r)
}

type operationWebSocket struct {
	req *http.Request
	op  *operations.Operation
}

func (r *operationWebSocket) Render(w http.ResponseWriter) error {
	chanErr, err := r.op.Connect(r.req, w)
	if err != nil {
		return err
	}

	err = <-chanErr
	return err
}

func (r *operationWebSocket) String() string {
	_, md, err := r.op.Render()
	if err != nil {
		return fmt.Sprintf("error: %s", err)
	}

	return md.ID
}

type forwardedOperationWebSocket struct {
	req    *http.Request
	id     string
	source *websocket.Conn // Connection to the node were the operation is running
}

func (r *forwardedOperationWebSocket) Render(w http.ResponseWriter) error {
	target, err := shared.WebsocketUpgrader.Upgrade(w, r.req, nil)
	if err != nil {
		return err
	}
	<-shared.WebsocketProxy(r.source, target)
	return nil
}

func (r *forwardedOperationWebSocket) String() string {
	return r.id
}

// swagger:operation GET /1.0/operations/{id}/websocket?public operations operation_websocket_get_untrusted
//
// Get the websocket stream
//
// Connects to an associated websocket stream for the operation.
// This should almost never be done directly by a client, instead it's
// meant for LXD to LXD communication with the client only relaying the
// connection information to the servers.
//
// The untrusted endpoint is used by the target server to connect to the source server.
// Authentication is performed through the secret token.
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: secret
//     description: Authentication token
//     type: string
//     example: random-string
// responses:
//   "200":
//     description: Websocket operation messages (dependent on operation)
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"

// swagger:operation GET /1.0/operations/{id}/websocket operations operation_websocket_get
//
// Get the websocket stream
//
// Connects to an associated websocket stream for the operation.
// This should almost never be done directly by a client, instead it's
// meant for LXD to LXD communication with the client only relaying the
// connection information to the servers.
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: secret
//     description: Authentication token
//     type: string
//     example: random-string
// responses:
//   "200":
//     description: Websocket operation messages (dependent on operation)
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func operationWebsocketGet(d *Daemon, r *http.Request) response.Response {
	id := mux.Vars(r)["id"]

	// First check if the query is for a local operation from this node
	op, err := operations.OperationGetInternal(id)
	if err == nil {
		return &operationWebSocket{r, op}
	}

	// Then check if the query is from an operation on another node, and, if so, forward it
	secret := r.FormValue("secret")
	if secret == "" {
		return response.BadRequest(fmt.Errorf("missing secret"))
	}

	var address string
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		operation, err := tx.GetOperationByUUID(id)
		if err != nil {
			return err
		}

		address = operation.NodeAddress
		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	cert := d.endpoints.NetworkCert()
	client, err := cluster.Connect(address, cert, false)
	if err != nil {
		return response.SmartError(err)
	}

	source, err := client.GetOperationWebsocket(id, secret)
	if err != nil {
		return response.SmartError(err)
	}

	return &forwardedOperationWebSocket{r, id, source}
}
