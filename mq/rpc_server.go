package mq

import (
	"encoding/json"
	"time"

	"github.com/chenyf/gibbon/comet"
	log "github.com/cihub/seelog"
	"github.com/streadway/amqp"
)

var (
	rpcExchangeType string = "direct"
)

type RpcServer struct {
	conn     *amqp.Connection
	channel  *amqp.Channel
	exchange string
}

func NewRpcServer(amqpURI, exchange, bindingKey string) (*RpcServer, error) {
	server := &RpcServer{
		exchange: exchange,
	}

	var err error
	server.conn, err = amqp.Dial(amqpURI)
	if err != nil {
		log.Errorf("Dial: %s", err)
		return nil, err
	}

	log.Infof("got Connection, getting Channel")
	server.channel, err = server.conn.Channel()
	if err != nil {
		log.Errorf("Channel: %s", err)
		return nil, err
	}

	log.Infof("got Channel, declaring %q Exchange (%q)", rpcExchangeType, exchange)

	if err := server.channel.ExchangeDeclare(
		exchange,        // name
		rpcExchangeType, // type
		true,            // durable
		false,           // auto-deleted
		false,           // internal
		false,           // noWait
		nil,             // arguments
	); err != nil {
		log.Errorf("Exchange Declare: %s", err)
		return nil, err
	}

	rpcQueue, err := server.channel.QueueDeclare(
		"",    // name
		false, // durable
		true,  // autoDelete
		true,  // exclusive
		false, // noWait
		nil,   // args
	)
	if err != nil {
		log.Errorf("Queue Declare: %s", err)
		return nil, err
	}
	log.Infof("declared RPC queue [%s]", rpcQueue.Name)

	if err = server.channel.QueueBind(
		rpcQueue.Name, // name of the queue
		bindingKey,    // bindingKey
		exchange,      // sourceExchange
		false,         // noWait
		nil,           // arguments
	); err != nil {
		log.Errorf("Queue bind: %s", err)
		return nil, err
	}

	deliveries, err := server.channel.Consume(
		rpcQueue.Name, // name
		"",            // consumerTag,
		false,         // noAck
		false,         // exclusive
		false,         // noLocal
		false,         // noWait
		nil,           // arguments
	)
	if err != nil {
		log.Errorf("consume error: %s", err)
		return nil, err
	}

	go server.handleDeliveries(deliveries)

	return server, nil
}

func (this *RpcServer) Stop() {
	this.conn.Close()
}

func (this *RpcServer) SendRpcResponse(callbackQueue, correlationId string, resp interface{}) {
	log.Infof("Sending RPC reply. RequestId: %s", correlationId)
	data, _ := json.Marshal(resp)
	if err := this.channel.Publish(
		"",            // publish to an exchange
		callbackQueue, // routingKey
		false,         // mandatory
		false,         // immediate
		amqp.Publishing{
			Headers:         amqp.Table{},
			ContentType:     "text/plain",
			ContentEncoding: "",
			DeliveryMode:    amqp.Transient, // 1=non-persistent, 2=persistent
			Priority:        0,              // 0-9
			ReplyTo:         "",
			CorrelationId:   correlationId,
			Body:            data,
		},
	); err != nil {
		log.Errorf("Exchange Publish: %s", err)
	}
}

func (this *RpcServer) handleDeliveries(deliveries <-chan amqp.Delivery) {
	for d := range deliveries {
		log.Debugf("got %dB RPC request [%s]", len(d.Body), d.CorrelationId)
		d.Ack(false)

		var msg MQ_Msg_Crtl
		if err := json.Unmarshal(d.Body, &msg); err != nil {
			log.Errorf("Unknown MQ message: %s", err)
			continue
		}

		go this.handleRpcRequest(&msg, d.ReplyTo, d.CorrelationId)
	}

	log.Infof("handle: deliveries channel closed")
	//done <- nil
}

func (this *RpcServer) handleRpcRequest(msg *MQ_Msg_Crtl, replyTo, correlationId string) {
	rpcReply := MQ_Msg_CtrlReply{}
	c := comet.DevMap.Get(msg.DeviceId)
	if c == nil {
		log.Warnf("RPC: no device %s on this server.", msg.DeviceId)
		rpcReply.Status = STATUS_NO_DEVICE
		this.SendRpcResponse(replyTo, correlationId, rpcReply)
		return
	}
	client := c.(*comet.Client)
	var replyChannel chan *comet.Message = nil
	wait := 10
	replyChannel = make(chan *comet.Message)
	seq := client.SendMessage(comet.MSG_ROUTER_COMMAND, []byte(msg.Cmd), replyChannel)
	select {
	case reply := <-replyChannel:
		rpcReply.Status = 0
		rpcReply.Result = string(reply.Data)
		this.SendRpcResponse(replyTo, correlationId, rpcReply)
		return
	case <-time.After(time.Duration(wait) * time.Second):
		log.Warnf("MSG timeout. RequestId: %s, seq: %d", correlationId, seq)
		client.MsgTimeout(seq)
		rpcReply.Status = STATUS_SEND_TIMEOUT
		this.SendRpcResponse(replyTo, correlationId, rpcReply)
		return
	}
}
