package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/streadway/amqp"
	"github.com/tywin1104/mc-gatekeeper/cache"
	"github.com/tywin1104/mc-gatekeeper/db"
	"github.com/tywin1104/mc-gatekeeper/mailer"
	"github.com/tywin1104/mc-gatekeeper/rcon"
	"github.com/tywin1104/mc-gatekeeper/types"
	"github.com/tywin1104/mc-gatekeeper/utils"
	"go.mongodb.org/mongo-driver/bson"
	try "gopkg.in/matryer/try.v1"
)

// Worker defines message queue worker
type Worker struct {
	dbService        *db.Service
	cache            *cache.Service
	logger           *logrus.Entry
	rconClient       *rcon.Client
	conn             *amqp.Connection
	channel          *amqp.Channel
	rabbitCloseError chan *amqp.Error
	delivery         <-chan amqp.Delivery
}

// NewWorker creates a worker to constantly listen and handle messages in the queue.
// At this step, the worker will establish connection to the message queue and rconClient.
func NewWorker(db *db.Service, cache *cache.Service, logger *logrus.Entry, rabbitCloseError chan *amqp.Error) (*Worker, error) {
	// Initialize rcon client to interact with game server
	var rconClient *rcon.Client
	if viper.GetString("environment") == "test" {
		// For testing environment do not connect to a running game server
		rconClient = nil
	} else {
		var err error
		rconClient, err = rcon.NewClient(viper.GetString("RCONServer"), viper.GetInt("RCONPort"), viper.GetString("RCONPassword"))
		if err != nil {
			return nil, err
		}
	}
	return &Worker{
		dbService:        db,
		cache:            cache,
		logger:           logger,
		rconClient:       rconClient,
		rabbitCloseError: rabbitCloseError,
	}, nil
}

func (worker *Worker) failOnError(err error, msg string) {
	if err != nil {
		worker.logger.WithFields(logrus.Fields{
			"err": err,
		}).Fatal(msg)
	}
}

func (w *Worker) GetConn() *amqp.Connection {
	return w.conn
}
func (w *Worker) GetChannel() *amqp.Channel {
	return w.channel
}

// Close connection and channel associated with the worker
func (worker *Worker) Close() {
	worker.channel.Close()
	worker.conn.Close()
}

// Start the worker to process the messages pushed into the queue
func (worker *Worker) Start(wg *sync.WaitGroup) {
	log := worker.logger

	// Start will only perform initial setup
	// If in the future the initial connection got closed,
	// reconnect callback function will be executed and conn/chan/chan *Error will be reset
	conn, err := amqp.Dial(viper.GetString("rabbitMQConn"))
	worker.failOnError(err, "Failed to connect to RabbitMQ")
	worker.conn = conn
	worker.conn.NotifyClose(worker.rabbitCloseError)

	ch, err := conn.Channel()
	worker.failOnError(err, "Failed to open a channel")
	worker.channel = ch
	args := make(amqp.Table)
	// Dead letter exchange name
	args["x-dead-letter-exchange"] = "retry.ex"
	// Default message ttl 24 hours
	args["x-message-ttl"] = int32(8.64e+7)

	err = ch.ExchangeDeclare(
		"work.ex", // name
		"fanout",  // type
		true,      // durable
		false,     // auto-deleted
		false,     // internal
		false,     // no-wait
		nil,       // arguments
	)
	if err != nil {
		worker.failOnError(err, "")
	}

	_, err = ch.QueueDeclare(
		"work.queue", // name
		true,         // durable
		false,        // delete when unused
		false,        // exclusive
		false,        // no-wait
		args,         // arguments
	)
	if err != nil {
		worker.failOnError(err, "")
	}

	err = ch.QueueBind(
		"work.queue", // queue name
		"",           // routing key
		"work.ex",    // exchange
		false,
		nil,
	)
	if err != nil {
		worker.failOnError(err, "")
	}
	worker.failOnError(err, "Failed to declare a queue")

	err = ch.Qos(
		1,     // prefetch count
		0,     // prefetch size
		false, // global
	)
	worker.failOnError(err, "Failed to set QoS")

	forever := make(chan bool)
	// Set initial delivery channel from the initial connection
	worker.updateDeliveryChannel()
	// Start background job to collect aggregate stats at a interval
	go worker.aggregateStats()
	go worker.runLoop()
	log.Info("Worker started. Listening for messages..")
	wg.Done()

	<-forever
}

// Update the messages fetching origin to be from the channel of the new connection
// Is called whenver a new connection is established and the old one is closed
func (worker *Worker) updateDeliveryChannel() {
	msgs, err := worker.channel.Consume(
		"work.queue", // queue
		"",           // consumer
		false,        // auto-ack
		false,        // exclusive
		false,        // no-local
		false,        // no-wait
		nil,          // args
	)
	worker.failOnError(err, "Failed to register a consumer")
	worker.delivery = msgs
}

func (worker *Worker) reconnect() {
	worker.logger.Warning("Worker connection with message queue closed unexpectedly. About to reconnect")
	worker.rabbitCloseError = make(chan *amqp.Error)
	err := try.Do(func(attempt int) (bool, error) {
		if attempt > 1 {
			worker.logger.Infof("Trying to connect to RabbitMQ [%d/3]\n", attempt)
		}
		var e error
		conn, e := amqp.Dial(viper.GetString("rabbitMQConn"))
		if e != nil {
			time.Sleep(5 * time.Second)
		} else {
			worker.conn = conn
		}
		return attempt < 3, e
	})
	if err != nil {
		worker.logger.WithFields(logrus.Fields{
			"err": err.Error(),
		}).Fatal("Unable to reconnect to message queue")
	}

	ch, err := worker.conn.Channel()
	worker.failOnError(err, "Failed to open a channel")
	worker.channel = ch
	// Update worker's delivery from newly created channel of new connection
	worker.updateDeliveryChannel()
	worker.logger.Info("Worker-message queue connection established. Continue to process messages")
	worker.conn.NotifyClose(worker.rabbitCloseError)
}
func (worker *Worker) runLoop() {
	for {
		select {
		case rabbitErr := <-worker.rabbitCloseError:
			if rabbitErr != nil {
				worker.reconnect()
			}
			break
		case d := <-worker.delivery:
			log := worker.logger
			if d.Body == nil {
				break
			}
			whitelistRequest, err := deserialize(d.Body)
			if err != nil {
				log.WithFields(logrus.Fields{
					"messageBody": d.Body,
					"err":         err,
				}).Error("Unable to decode message into whitelistRequest")
				// Unable to decode this message, discard the message
				d.Ack(false)
			} else {
				// Concrete actions to do when receiving task from message queue
				// From the message body to determine which type of work to do
				switch whitelistRequest.Status {
				case types.StatusApproved:
					worker.processApproval(d, whitelistRequest)
				case types.StatusDenied:
					worker.processDenial(d, whitelistRequest)
				case types.StatusPending:
					worker.processNewRequest(d, whitelistRequest)
				case types.StatusDeactivated:
					worker.processDeactivate(d, whitelistRequest)
				case types.StatusBanned:
					worker.processBan(d, whitelistRequest)
				}
			}
		}
	}
}

func (worker *Worker) updateCache(request types.WhitelistRequest) {
	// Update the cache for all requests. Best effort only
	err := worker.cache.UpdateAllRequests()
	if err != nil {
		worker.logger.WithFields(logrus.Fields{
			"err": err.Error(),
		}).Warning("Unable to refresh all requests in cache")
	}

	// Update Stats value in cache
	err = worker.cache.UpdateRealTimeStats(request)
	if err != nil {
		worker.logger.WithFields(logrus.Fields{
			"err": err.Error(),
		}).Warning("Unable to update stats in cache")
	}
}

func (worker *Worker) processApproval(d amqp.Delivery, request types.WhitelistRequest) {
	worker.logger.WithFields(logrus.Fields{
		"username": request.Username,
		"ID":       request.ID,
		"Type":     "Approval Task",
	}).Info("Received new task")

	worker.updateCache(request)
	// Concrete whitelist action on the game server
	err := worker.issueRCON("whitelist add " + request.Username)
	if err != nil {
		worker.logger.WithFields(logrus.Fields{
			"username": request.Username,
			"err":      err.Error(),
		}).Warn("Unable to whitelist user at this moment. Will retry later.")
		worker.retryMsgWithDelay(d, "Whitelist "+request.Username)
		return
	}
	// Only send email to the user once, not in each retry
	if _, ok := d.Headers["x-retry-count"]; !ok {
		worker.emailDecision(request)
	}
	worker.updateOnserverStatus(request, "Whitelisted")
	d.Ack(false)
}

func (worker *Worker) processDenial(d amqp.Delivery, request types.WhitelistRequest) {
	// Need to send update status back to the user
	// Put message to dead letter queue for later investigation if unable to send decision email
	worker.logger.WithFields(logrus.Fields{
		"username": request.Username,
		"ID":       request.ID,
		"Type":     "Denial Task",
	}).Info("Received new task")

	worker.updateCache(request)
	worker.emailDecision(request)
	d.Ack(false)
}

// Ban will permanately ban a user from the server and woll prevent
// applications coming from that user
func (worker *Worker) processBan(d amqp.Delivery, request types.WhitelistRequest) {
	worker.logger.WithFields(logrus.Fields{
		"username": request.Username,
		"ID":       request.ID,
		"Type":     "Ban Task",
	}).Info("Received new task")
	err := worker.issueRCON("ban " + request.Username)
	if err != nil {
		worker.logger.WithFields(logrus.Fields{
			"username": request.Username,
			"err":      err.Error(),
		}).Warn("Unable to ban user on the game server at this moment. Will retry later.")
		// Republish the msg to the retry queue with incremented retryCount and exponential backoff expiration time
		worker.retryMsgWithDelay(d, "Ban "+request.Username)
	}
	worker.updateOnserverStatus(request, types.OnserverBanned)
	worker.updateCache(request)
	d.Ack(false)
}

// Deactivate a user will un-whitelist that username. But allow further applications
// from the same user
func (worker *Worker) processDeactivate(d amqp.Delivery, request types.WhitelistRequest) {
	worker.logger.WithFields(logrus.Fields{
		"username": request.Username,
		"ID":       request.ID,
		"Type":     "Deactivate Task",
	}).Info("Received new task")
	err := worker.issueRCON("whitelist remove " + request.Username)
	if err != nil {
		worker.logger.WithFields(logrus.Fields{
			"username": request.Username,
			"err":      err.Error(),
		}).Warn("Unable to deactivate user on the game server at this moment. Will retry later.")
		worker.retryMsgWithDelay(d, "Deactivate "+request.Username)
		return
	}
	worker.updateOnserverStatus(request, types.OnserverNone)
	worker.updateCache(request)
	d.Ack(false)
}

func (worker *Worker) processNewRequest(d amqp.Delivery, request types.WhitelistRequest) {
	worker.logger.WithFields(logrus.Fields{
		"username": request.Username,
		"ID":       request.ID,
		"Type":     "New Reqeust Task",
	}).Info("Received new task")

	// Need to handle new request
	// Send application confirmation email to user
	worker.emailConfirmation(request)

	// Send approval request emails to op(s)
	successCount, err := worker.emailToOps(request, viper.GetInt("minRequiredReceiver"))
	if err != nil {
		// If success count for sending ops emails less than minimum quoram, put to dead letter queue
		worker.logger.WithFields(logrus.Fields{
			"message":      request,
			"successCount": successCount,
		}).Error("Failed to dispatch action emails to required number of ops")
		return
	}
	worker.updateCache(request)
	d.Ack(false)
}

// retryMsgWithDelay will ack the original message and republish the message
// to the wait queue with exponential-backoff expiration value
func (worker *Worker) retryMsgWithDelay(d amqp.Delivery, actionDescription string) {
	var expiration int = 15 * 60 * 1000
	if val, ok := d.Headers["x-death"]; ok {
		val := val.([]interface{})
		if len(val) > 0 {
			expirationStr := val[0].(amqp.Table)["original-expiration"].(string)
			i, _ := strconv.Atoi(expirationStr)
			expiration = i * 2
		}
	}
	var retryCount int32 = 1
	if val, ok := d.Headers["x-retry-count"]; ok {
		retryCount = val.(int32) + 1
	}
	// If retry count reaches the maximum value, discard the message to prevent infinite loop
	if retryCount > 6 {
		worker.logger.WithFields(logrus.Fields{
			"action": actionDescription,
		}).Error("Unable to performe this operation. Give up on tries")
		d.Ack(false)
		return
	}
	headers := make(amqp.Table)
	headers["x-retry-count"] = retryCount
	e := worker.channel.Publish(
		"retry.ex", // exchange
		"",         // routing key
		false,      // mandatory
		false,
		amqp.Publishing{
			Headers:      headers,
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Expiration:   strconv.Itoa(expiration),
			Body:         d.Body,
		})
	if e != nil {
		worker.logger.WithFields(logrus.Fields{
			"err": e.Error(),
		}).Error("Unable to republish message for retry")
	}
}

func (worker *Worker) emailDecision(whitelistRequest types.WhitelistRequest) error {
	log := worker.logger
	requestIDToken, err := utils.EncodeAndEncrypt(whitelistRequest.ID.Hex(), viper.GetString("passphrase"))
	if err != nil {
		log.WithFields(logrus.Fields{
			"err": err,
		}).Error("Failed to encode requestID Token")
		return err
	}
	var subject string
	var template string
	if whitelistRequest.Status == types.StatusApproved {
		subject = viper.GetString("approvedEmailTitle")
		template = "./mailer/templates/approve.html"
	} else {
		subject = viper.GetString("deniedEmailTitle")
		template = "./mailer/templates/deny.html"
	}
	err = mailer.Send(template, map[string]string{"link": requestIDToken}, subject, whitelistRequest.Email)
	if err != nil {
		log.WithFields(logrus.Fields{
			"recipent": whitelistRequest.Email,
			"err":      err,
			"ID":       whitelistRequest.ID.Hex(),
		}).Error("Failed to send decision email")
	} else {
		log.WithFields(logrus.Fields{
			"recipent": whitelistRequest.Email,
		}).Info("Decision email sent")
	}
	return err
}

func (worker *Worker) emailConfirmation(whitelistRequest types.WhitelistRequest) error {
	log := worker.logger
	subject := viper.GetString("confirmationEmailTitle")
	requestIDToken, err := utils.EncodeAndEncrypt(whitelistRequest.ID.Hex(), viper.GetString("passphrase"))
	if err != nil {
		log.WithFields(logrus.Fields{
			"err": err,
		}).Error("Failed to encode requestID Token")
		return err
	}
	confirmationLink := os.Getenv("FRONTEND_DEPLOYED_URL") + "status/" + requestIDToken
	err = mailer.Send("./mailer/templates/confirmation.html", map[string]string{"link": confirmationLink}, subject, whitelistRequest.Email)
	if err != nil {
		log.WithFields(logrus.Fields{
			"recipent": whitelistRequest.Email,
			"err":      err,
			"ID":       whitelistRequest.ID.Hex(),
		}).Error("Failed to send confirmation email")
	} else {
		log.WithFields(logrus.Fields{
			"recipent": whitelistRequest.Email,
		}).Info("Confirmation email sent")
	}
	return err
}

func (worker *Worker) emailToOps(whitelistRequest types.WhitelistRequest, quoram int) (int, error) {
	log := worker.logger
	subject := "[Action Required] Whitelist request from " + whitelistRequest.Username
	successCount := 0
	requestIDToken, err := utils.EncodeAndEncrypt(whitelistRequest.ID.Hex(), viper.GetString("passphrase"))
	if err != nil {
		log.WithFields(logrus.Fields{
			"err": err,
		}).Error("Failed to encode requestID Token")
		return 0, err
	}
	// ops who received the action emails successfully will be added to the assignees
	// and attach as the metadata for the request db object
	assignees := []string{}
	// Get target ops to send action emails according to the configured dispatching strategy
	ops := worker.getTargetOps()
	for _, op := range ops {
		opEmailToken, err := utils.EncodeAndEncrypt(op, viper.GetString("passphrase"))
		if err != nil {
			log.WithFields(logrus.Fields{
				"err": err,
			}).Error("Failed to encode opEmail Token")
			return 0, err
		}
		opLink := os.Getenv("FRONTEND_DEPLOYED_URL") + "action/" + requestIDToken + "?adm=" + opEmailToken
		err = mailer.Send("./mailer/templates/ops.html", map[string]string{"link": opLink}, subject, op)
		if err != nil {
			log.WithFields(logrus.Fields{
				"recipent": op,
				"err":      err,
				"ID":       whitelistRequest.ID.Hex(),
			}).Error("Failed to send email to op")
		} else {
			log.WithFields(logrus.Fields{
				"recipent": op,
				"ID":       whitelistRequest.ID.Hex(),
			}).Info("Action email sent to op")
			assignees = append(assignees, op)
			successCount++
		}
	}
	// Attach assignee info to the db request object to keep track of each request
	if len(assignees) > 0 {
		requestedChange := make(bson.M)
		requestedChange["assignees"] = assignees
		_, err := worker.dbService.UpdateRequest(bson.D{{"_id", whitelistRequest.ID}}, bson.M{
			"$set": requestedChange,
		})
		if err != nil {
			log.WithFields(logrus.Fields{
				"err":       err,
				"assignees": assignees,
				"ID":        whitelistRequest.ID.Hex(),
			}).Error("Unable to update request db object with assignees metadata")
		}
	}
	if successCount >= quoram {
		return successCount, nil
	}
	return successCount, errors.New("Success count does not reach minimum requirement")
}

func (worker *Worker) updateOnserverStatus(request types.WhitelistRequest, onserverStatus string) {
	requestedChange := make(bson.M)
	requestedChange["onserverStatus"] = onserverStatus
	_, err := worker.dbService.UpdateRequest(bson.D{{"_id", request.ID}}, bson.M{
		"$set": requestedChange,
	})
	if err != nil {
		worker.logger.WithFields(logrus.Fields{
			"err": err,
			"ID":  request.ID.Hex(),
		}).Error("Unable to update onserver status")
	}
}

func (worker *Worker) getTargetOps() []string {
	// Strategy: Broadcast / Random with threshold
	ops := viper.GetStringSlice("ops")
	if viper.GetString("dispatchingStrategy") == "Broadcast" {
		return ops
	}
	n := viper.GetInt("randomDispatchingThreshold")
	// Choose random n out of all ops as the target request handlers
	rand.Seed(time.Now().UnixNano())
	rand.Shuffle(len(ops), func(i, j int) { ops[i], ops[j] = ops[j], ops[i] })
	return ops[:n]
}

// issue  command againest a user on the game server with retries
func (worker *Worker) issueRCON(command string) error {
	_, err := worker.rconClient.SendCommand(command)

	if err != nil {
		return err
	}
	worker.logger.WithFields(logrus.Fields{
		"command": command,
	}).Info("Command has been issued successfully on the game server")
	return nil
}

// aggregateStats will compute and update the cache value for aggregate stats periodically
func (worker *Worker) aggregateStats() {
	log := worker.logger
	for range time.Tick(60 * time.Second) {
		go func() {
			err := worker.cache.UpdateAggregateStats()
			if err != nil {
				log.WithFields(logrus.Fields{
					"err": err.Error(),
				}).Error("Unable to aggregate stats")
			} else {
				log.Info("Aggregate stats data completed")
			}
		}()
	}
}

func deserialize(b []byte) (types.WhitelistRequest, error) {
	var msg types.WhitelistRequest
	buf := bytes.NewBuffer(b)
	decoder := json.NewDecoder(buf)
	err := decoder.Decode(&msg)
	return msg, err
}
