package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/streadway/amqp"
	"github.com/tywin1104/mc-whitelist/config"
	"github.com/tywin1104/mc-whitelist/db"
	"github.com/tywin1104/mc-whitelist/mailer"
	"github.com/tywin1104/mc-whitelist/types"
	"github.com/tywin1104/mc-whitelist/utils"
	"go.mongodb.org/mongo-driver/bson"
)

// Worker defines message queue worker
type Worker struct {
	dbService *db.Service
	c         *config.Config
	logger    *logrus.Entry
}

// NewWorker creates a worker to constantly listen and handle messages in the queue
func NewWorker(db *db.Service, c *config.Config, logger *logrus.Entry) *Worker {
	return &Worker{
		dbService: db,
		c:         c,
		logger:    logger,
	}
}

func (worker *Worker) failOnError(err error, msg string) {
	if err != nil {
		worker.logger.WithFields(logrus.Fields{
			"err": err,
		}).Fatal(msg)
	}
}

// Start the worker to process the messages pushed into the queue
func (worker *Worker) Start(wg *sync.WaitGroup) {
	log := worker.logger
	config, err := config.LoadConfig()
	worker.failOnError(err, "Failed to load config")

	conn, err := amqp.Dial(config.RabbitmqConnStr)
	worker.failOnError(err, "Failed to connect to RabbitMQ")
	defer conn.Close()

	ch, err := conn.Channel()
	worker.failOnError(err, "Failed to open a channel")
	defer ch.Close()

	args := make(amqp.Table)
	// Dead letter exchange name
	args["x-dead-letter-exchange"] = "dead.letter.ex"
	// Default message ttl 24 hours
	args["x-message-ttl"] = int32(8.64e+7)

	q, err := ch.QueueDeclare(
		config.TaskQueueName, // name
		true,                 // durable
		false,                // delete when unused
		false,                // exclusive
		false,                // no-wait
		args,                 // arguments
	)
	worker.failOnError(err, "Failed to declare a queue")

	err = ch.Qos(
		1,     // prefetch count
		0,     // prefetch size
		false, // global
	)
	worker.failOnError(err, "Failed to set QoS")

	msgs, err := ch.Consume(
		q.Name, // queue
		"",     // consumer
		false,  // auto-ack
		false,  // exclusive
		false,  // no-local
		false,  // no-wait
		nil,    // args
	)
	worker.failOnError(err, "Failed to register a consumer")

	forever := make(chan bool)
	go worker.runLoop(msgs)
	log.Info("Worker started. Listening for messages..")
	wg.Done()

	<-forever
}

func (worker *Worker) runLoop(msgs <-chan amqp.Delivery) {
	log := worker.logger
	for d := range msgs {
		whitelistRequest, err := deserialize(d.Body)
		if err != nil {
			log.WithFields(logrus.Fields{
				"messageBody": d.Body,
				"err":         err,
			}).Error("Unable to decode message into whitelistRequest")
			// Unable to decode this message, put to the dead-letter queue
			d.Nack(false, false)
		} else {
			// Concrete actions to do when receiving task from message queue
			// From the message body to determine which type of work to do
			switch whitelistRequest.Status {
			case "Approved":
				worker.processApproval(d, whitelistRequest)
			case "Denied":
				worker.processDenial(d, whitelistRequest)
			case "Pending":
				worker.processNewRequest(d, whitelistRequest)
			}
		}
	}
}

// Nack if decision email is not sent. Ack if sent.
func (worker *Worker) processApproval(d amqp.Delivery, request types.WhitelistRequest) {
	worker.logger.WithFields(logrus.Fields{
		"username": request.Username,
		"ID":       request.ID,
		"Type":     "Approval Task",
	}).Info("Received new task")
	err := worker.emailDecision(request)
	if err != nil {
		d.Nack(false, false)
		return
	}
	// TODO: Add interface to do concrete whitelist action on the game server
	d.Ack(false)
}

// Nack if decision email is not sent. Ack if sent.
func (worker *Worker) processDenial(d amqp.Delivery, request types.WhitelistRequest) {
	// Need to send update status back to the user
	// Put message to dead letter queue for later investigation if unable to send decision email
	worker.logger.WithFields(logrus.Fields{
		"username": request.Username,
		"ID":       request.ID,
		"Type":     "Denial Task",
	}).Info("Received new task")
	err := worker.emailDecision(request)
	if err != nil {
		d.Nack(false, false)
		return
	}
	d.Ack(false)
}

//Nack: successful ops emails less than threshold; confirmation email does not count
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
	successCount, err := worker.emailToOps(request, 1)
	if err != nil {
		// If success count for sending ops emails less than minimum quoram, put to dead letter queue
		worker.logger.WithFields(logrus.Fields{
			"message":      request,
			"successCount": successCount,
		}).Error("Failed to dispatch action emails to required number of ops")
		d.Nack(false, false)
		return
	}
	d.Ack(false)
}

func (worker *Worker) emailDecision(whitelistRequest types.WhitelistRequest) error {
	log := worker.logger
	requestIDToken, err := utils.EncodeAndEncrypt(whitelistRequest.ID.Hex(), worker.c.PassPhrase)
	if err != nil {
		log.WithFields(logrus.Fields{
			"err": err,
		}).Error("Failed to encode requestID Token")
		return err
	}
	var subject string
	var template string
	if whitelistRequest.Status == "Approved" {
		subject = "Your request to join the server is approved"
		template = "./mailer/templates/approve.html"
	} else {
		subject = "Update regarding your request to join the server"
		template = "./mailer/templates/deny.html"
	}
	err = mailer.Send(template, map[string]string{"link": requestIDToken}, subject, whitelistRequest.Email, worker.c)
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
	subject := "Your request to join the server has been received"
	requestIDToken, err := utils.EncodeAndEncrypt(whitelistRequest.ID.Hex(), worker.c.PassPhrase)
	if err != nil {
		log.WithFields(logrus.Fields{
			"err": err,
		}).Error("Failed to encode requestID Token")
		return err
	}
	confirmationLink := os.Getenv("FRONTEND_DEPLOYED_URL") + "status/" + requestIDToken
	err = mailer.Send("./mailer/templates/confirmation.html", map[string]string{"link": confirmationLink}, subject, whitelistRequest.Email, worker.c)
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
	requestIDToken, err := utils.EncodeAndEncrypt(whitelistRequest.ID.Hex(), worker.c.PassPhrase)
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
		opEmailToken, err := utils.EncodeAndEncrypt(op, worker.c.PassPhrase)
		if err != nil {
			log.WithFields(logrus.Fields{
				"err": err,
			}).Error("Failed to encode opEmail Token")
			return 0, err
		}
		opLink := os.Getenv("FRONTEND_DEPLOYED_URL") + "action/" + requestIDToken + "?adm=" + opEmailToken
		err = mailer.Send("./mailer/templates/ops.html", map[string]string{"link": opLink}, subject, op, worker.c)
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

func (worker *Worker) getTargetOps() []string {
	// Strategy: Broadcast / Random with threshold
	ops := worker.c.Ops
	if worker.c.DispatchingStrategy == "Broadcast" {
		return ops
	}
	n := worker.c.RandomDispatchingThreshold
	// Choose random n out of all ops as the target request handlers
	rand.Seed(time.Now().UnixNano())
	rand.Shuffle(len(ops), func(i, j int) { ops[i], ops[j] = ops[j], ops[i] })
	return ops[:n]
}

func deserialize(b []byte) (types.WhitelistRequest, error) {
	var msg types.WhitelistRequest
	buf := bytes.NewBuffer(b)
	decoder := json.NewDecoder(buf)
	err := decoder.Decode(&msg)
	return msg, err
}
