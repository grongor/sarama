package sarama

import (
	"log"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testMsg = StringEncoder("Foo")

// If a particular offset is provided then messages are consumed starting from
// that offset.
func TestConsumerOffsetManual(t *testing.T) {
	// Given
	broker0 := NewMockBroker(t, 0)

	mockFetchResponse := NewMockFetchResponse(t, 1)
	for i := 0; i < 10; i++ {
		mockFetchResponse.SetMessage("my_topic", 0, int64(i+1234), testMsg)
	}

	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, OffsetOldest, 0).
			SetOffset("my_topic", 0, OffsetNewest, 2345),
		"FetchRequest": mockFetchResponse,
	})

	// When
	master, err := NewConsumer([]string{broker0.Addr()}, NewTestConfig())
	if err != nil {
		t.Fatal(err)
	}

	consumer, err := master.ConsumePartition("my_topic", 0, 1234)
	if err != nil {
		t.Fatal(err)
	}

	// Then: messages starting from offset 1234 are consumed.
	for i := 0; i < 10; i++ {
		select {
		case message := <-consumer.Messages():
			assertMessageOffset(t, message, int64(i+1234))
		case err := <-consumer.Errors():
			t.Error(err)
		}
	}

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()
}

// If `OffsetNewest` is passed as the initial offset then the first consumed
// message is indeed corresponds to the offset that broker claims to be the
// newest in its metadata response.
func TestConsumerOffsetNewest(t *testing.T) {
	// Given
	broker0 := NewMockBroker(t, 0)
	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, OffsetNewest, 10).
			SetOffset("my_topic", 0, OffsetOldest, 7),
		"FetchRequest": NewMockFetchResponse(t, 1).
			SetMessage("my_topic", 0, 9, testMsg).
			SetMessage("my_topic", 0, 10, testMsg).
			SetMessage("my_topic", 0, 11, testMsg).
			SetHighWaterMark("my_topic", 0, 14),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, NewTestConfig())
	if err != nil {
		t.Fatal(err)
	}

	// When
	consumer, err := master.ConsumePartition("my_topic", 0, OffsetNewest)
	if err != nil {
		t.Fatal(err)
	}

	// Then
	assertMessageOffset(t, <-consumer.Messages(), 10)
	if hwmo := consumer.HighWaterMarkOffset(); hwmo != 14 {
		t.Errorf("Expected high water mark offset 14, found %d", hwmo)
	}

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()
}

// It is possible to close a partition consumer and create the same anew.
func TestConsumerRecreate(t *testing.T) {
	// Given
	broker0 := NewMockBroker(t, 0)
	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, OffsetOldest, 0).
			SetOffset("my_topic", 0, OffsetNewest, 1000),
		"FetchRequest": NewMockFetchResponse(t, 1).
			SetMessage("my_topic", 0, 10, testMsg),
	})

	c, err := NewConsumer([]string{broker0.Addr()}, NewTestConfig())
	if err != nil {
		t.Fatal(err)
	}

	pc, err := c.ConsumePartition("my_topic", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	assertMessageOffset(t, <-pc.Messages(), 10)

	// When
	safeClose(t, pc)
	pc, err = c.ConsumePartition("my_topic", 0, 10)
	if err != nil {
		t.Fatal(err)
	}

	// Then
	assertMessageOffset(t, <-pc.Messages(), 10)

	safeClose(t, pc)
	safeClose(t, c)
	broker0.Close()
}

// An attempt to consume the same partition twice should fail.
func TestConsumerDuplicate(t *testing.T) {
	// Given
	broker0 := NewMockBroker(t, 0)
	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, OffsetOldest, 0).
			SetOffset("my_topic", 0, OffsetNewest, 1000),
		"FetchRequest": NewMockFetchResponse(t, 1),
	})

	config := NewTestConfig()
	config.ChannelBufferSize = 0
	c, err := NewConsumer([]string{broker0.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	pc1, err := c.ConsumePartition("my_topic", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	// When
	pc2, err := c.ConsumePartition("my_topic", 0, 0)

	// Then
	if pc2 != nil || err != ConfigurationError("That topic/partition is already being consumed") {
		t.Fatal("A partition cannot be consumed twice at the same time")
	}

	safeClose(t, pc1)
	safeClose(t, c)
	broker0.Close()
}

func runConsumerLeaderRefreshErrorTestWithConfig(t *testing.T, config *Config) {
	// Given
	broker0 := NewMockBroker(t, 100)

	// Stage 1: my_topic/0 served by broker0
	Logger.Printf("    STAGE 1")

	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, OffsetOldest, 123).
			SetOffset("my_topic", 0, OffsetNewest, 1000),
		"FetchRequest": NewMockFetchResponse(t, 1).
			SetMessage("my_topic", 0, 123, testMsg),
	})

	c, err := NewConsumer([]string{broker0.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	pc, err := c.ConsumePartition("my_topic", 0, OffsetOldest)
	if err != nil {
		t.Fatal(err)
	}

	assertMessageOffset(t, <-pc.Messages(), 123)

	// Stage 2: broker0 says that it is no longer the leader for my_topic/0,
	// but the requests to retrieve metadata fail with network timeout.
	Logger.Printf("    STAGE 2")

	fetchResponse2 := &FetchResponse{}
	fetchResponse2.AddError("my_topic", 0, ErrNotLeaderForPartition)

	broker0.SetHandlerByMap(map[string]MockResponse{
		"FetchRequest": NewMockWrapper(fetchResponse2),
	})

	if consErr := <-pc.Errors(); consErr.Err != ErrOutOfBrokers {
		t.Errorf("Unexpected error: %v", consErr.Err)
	}

	// Stage 3: finally the metadata returned by broker0 tells that broker1 is
	// a new leader for my_topic/0. Consumption resumes.

	Logger.Printf("    STAGE 3")

	broker1 := NewMockBroker(t, 101)

	broker1.SetHandlerByMap(map[string]MockResponse{
		"FetchRequest": NewMockFetchResponse(t, 1).
			SetMessage("my_topic", 0, 124, testMsg),
	})
	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetBroker(broker1.Addr(), broker1.BrokerID()).
			SetLeader("my_topic", 0, broker1.BrokerID()),
	})

	assertMessageOffset(t, <-pc.Messages(), 124)

	safeClose(t, pc)
	safeClose(t, c)
	broker1.Close()
	broker0.Close()
}

// If consumer fails to refresh metadata it keeps retrying with frequency
// specified by `Config.Consumer.Retry.Backoff`.
func TestConsumerLeaderRefreshError(t *testing.T) {
	config := NewTestConfig()
	config.Net.ReadTimeout = 100 * time.Millisecond
	config.Consumer.Retry.Backoff = 200 * time.Millisecond
	config.Consumer.Return.Errors = true
	config.Metadata.Retry.Max = 0

	runConsumerLeaderRefreshErrorTestWithConfig(t, config)
}

func TestConsumerLeaderRefreshErrorWithBackoffFunc(t *testing.T) {
	var calls int32 = 0

	config := NewTestConfig()
	config.Net.ReadTimeout = 100 * time.Millisecond
	config.Consumer.Retry.BackoffFunc = func(retries int) time.Duration {
		atomic.AddInt32(&calls, 1)
		return 200 * time.Millisecond
	}
	config.Consumer.Return.Errors = true
	config.Metadata.Retry.Max = 0

	runConsumerLeaderRefreshErrorTestWithConfig(t, config)

	// we expect at least one call to our backoff function
	if calls == 0 {
		t.Fail()
	}
}

func TestConsumerInvalidTopic(t *testing.T) {
	// Given
	broker0 := NewMockBroker(t, 100)
	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()),
	})

	c, err := NewConsumer([]string{broker0.Addr()}, NewTestConfig())
	if err != nil {
		t.Fatal(err)
	}

	// When
	pc, err := c.ConsumePartition("my_topic", 0, OffsetOldest)

	// Then
	if pc != nil || err != ErrUnknownTopicOrPartition {
		t.Errorf("Should fail with, err=%v", err)
	}

	safeClose(t, c)
	broker0.Close()
}

// Nothing bad happens if a partition consumer that has no leader assigned at
// the moment is closed.
func TestConsumerClosePartitionWithoutLeader(t *testing.T) {
	// Given
	broker0 := NewMockBroker(t, 100)
	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, OffsetOldest, 123).
			SetOffset("my_topic", 0, OffsetNewest, 1000),
		"FetchRequest": NewMockFetchResponse(t, 1).
			SetMessage("my_topic", 0, 123, testMsg),
	})

	config := NewTestConfig()
	config.Net.ReadTimeout = 100 * time.Millisecond
	config.Consumer.Retry.Backoff = 100 * time.Millisecond
	config.Consumer.Return.Errors = true
	config.Metadata.Retry.Max = 0
	c, err := NewConsumer([]string{broker0.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	pc, err := c.ConsumePartition("my_topic", 0, OffsetOldest)
	if err != nil {
		t.Fatal(err)
	}

	assertMessageOffset(t, <-pc.Messages(), 123)

	// broker0 says that it is no longer the leader for my_topic/0, but the
	// requests to retrieve metadata fail with network timeout.
	fetchResponse2 := &FetchResponse{}
	fetchResponse2.AddError("my_topic", 0, ErrNotLeaderForPartition)

	broker0.SetHandlerByMap(map[string]MockResponse{
		"FetchRequest": NewMockWrapper(fetchResponse2),
	})

	// When
	if consErr := <-pc.Errors(); consErr.Err != ErrOutOfBrokers {
		t.Errorf("Unexpected error: %v", consErr.Err)
	}

	// Then: the partition consumer can be closed without any problem.
	safeClose(t, pc)
	safeClose(t, c)
	broker0.Close()
}

// If the initial offset passed on partition consumer creation is out of the
// actual offset range for the partition, then the partition consumer stops
// immediately closing its output channels.
func TestConsumerShutsDownOutOfRange(t *testing.T) {
	// Given
	broker0 := NewMockBroker(t, 0)
	fetchResponse := new(FetchResponse)
	fetchResponse.AddError("my_topic", 0, ErrOffsetOutOfRange)
	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, OffsetNewest, 1234).
			SetOffset("my_topic", 0, OffsetOldest, 7),
		"FetchRequest": NewMockWrapper(fetchResponse),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, NewTestConfig())
	if err != nil {
		t.Fatal(err)
	}

	// When
	consumer, err := master.ConsumePartition("my_topic", 0, 101)
	if err != nil {
		t.Fatal(err)
	}

	// Then: consumer should shut down closing its messages and errors channels.
	if _, ok := <-consumer.Messages(); ok {
		t.Error("Expected the consumer to shut down")
	}
	safeClose(t, consumer)

	safeClose(t, master)
	broker0.Close()
}

// If a fetch response contains messages with offsets that are smaller then
// requested, then such messages are ignored.
func TestConsumerExtraOffsets(t *testing.T) {
	// Given
	legacyFetchResponse := &FetchResponse{}
	legacyFetchResponse.AddMessage("my_topic", 0, nil, testMsg, 1)
	legacyFetchResponse.AddMessage("my_topic", 0, nil, testMsg, 2)
	legacyFetchResponse.AddMessage("my_topic", 0, nil, testMsg, 3)
	legacyFetchResponse.AddMessage("my_topic", 0, nil, testMsg, 4)
	newFetchResponse := &FetchResponse{Version: 4}
	newFetchResponse.AddRecord("my_topic", 0, nil, testMsg, 1)
	newFetchResponse.AddRecord("my_topic", 0, nil, testMsg, 2)
	newFetchResponse.AddRecord("my_topic", 0, nil, testMsg, 3)
	newFetchResponse.AddRecord("my_topic", 0, nil, testMsg, 4)
	newFetchResponse.SetLastOffsetDelta("my_topic", 0, 4)
	newFetchResponse.SetLastStableOffset("my_topic", 0, 4)
	for _, fetchResponse1 := range []*FetchResponse{legacyFetchResponse, newFetchResponse} {
		var offsetResponseVersion int16
		cfg := NewTestConfig()
		cfg.Consumer.Return.Errors = true
		if fetchResponse1.Version >= 4 {
			cfg.Version = V0_11_0_0
			offsetResponseVersion = 1
		}

		broker0 := NewMockBroker(t, 0)
		fetchResponse2 := &FetchResponse{}
		fetchResponse2.Version = fetchResponse1.Version
		fetchResponse2.AddError("my_topic", 0, ErrNoError)
		broker0.SetHandlerByMap(map[string]MockResponse{
			"MetadataRequest": NewMockMetadataResponse(t).
				SetBroker(broker0.Addr(), broker0.BrokerID()).
				SetLeader("my_topic", 0, broker0.BrokerID()),
			"OffsetRequest": NewMockOffsetResponse(t).
				SetVersion(offsetResponseVersion).
				SetOffset("my_topic", 0, OffsetNewest, 1234).
				SetOffset("my_topic", 0, OffsetOldest, 0),
			"FetchRequest": NewMockSequence(fetchResponse1, fetchResponse2),
		})

		master, err := NewConsumer([]string{broker0.Addr()}, cfg)
		if err != nil {
			t.Fatal(err)
		}

		// When
		consumer, err := master.ConsumePartition("my_topic", 0, 3)
		if err != nil {
			t.Fatal(err)
		}

		// Then: messages with offsets 1 and 2 are not returned even though they
		// are present in the response.
		select {
		case msg := <-consumer.Messages():
			assertMessageOffset(t, msg, 3)
		case err := <-consumer.Errors():
			t.Fatal(err)
		}

		select {
		case msg := <-consumer.Messages():
			assertMessageOffset(t, msg, 4)
		case err := <-consumer.Errors():
			t.Fatal(err)
		}

		safeClose(t, consumer)
		safeClose(t, master)
		broker0.Close()
	}
}

// In some situations broker may return a block containing only
// messages older then requested, even though there would be
// more messages if higher offset was requested.
func TestConsumerReceivingFetchResponseWithTooOldRecords(t *testing.T) {
	// Given
	fetchResponse1 := &FetchResponse{Version: 4}
	fetchResponse1.AddRecord("my_topic", 0, nil, testMsg, 1)

	fetchResponse2 := &FetchResponse{Version: 4}
	fetchResponse2.AddRecord("my_topic", 0, nil, testMsg, 1000000)

	cfg := NewTestConfig()
	cfg.Consumer.Return.Errors = true
	cfg.Version = V0_11_0_0

	broker0 := NewMockBroker(t, 0)

	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetVersion(1).
			SetOffset("my_topic", 0, OffsetNewest, 1234).
			SetOffset("my_topic", 0, OffsetOldest, 0),
		"FetchRequest": NewMockSequence(fetchResponse1, fetchResponse2),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// When
	consumer, err := master.ConsumePartition("my_topic", 0, 2)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-consumer.Messages():
		assertMessageOffset(t, msg, 1000000)
	case err := <-consumer.Errors():
		t.Fatal(err)
	}

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()
}

func TestConsumeMessageWithNewerFetchAPIVersion(t *testing.T) {
	// Given
	fetchResponse1 := &FetchResponse{Version: 4}
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 1)
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 2)

	cfg := NewTestConfig()
	cfg.Version = V0_11_0_0

	broker0 := NewMockBroker(t, 0)
	fetchResponse2 := &FetchResponse{}
	fetchResponse2.Version = 4
	fetchResponse2.AddError("my_topic", 0, ErrNoError)
	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetVersion(1).
			SetOffset("my_topic", 0, OffsetNewest, 1234).
			SetOffset("my_topic", 0, OffsetOldest, 0),
		"FetchRequest": NewMockSequence(fetchResponse1, fetchResponse2),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// When
	consumer, err := master.ConsumePartition("my_topic", 0, 1)
	if err != nil {
		t.Fatal(err)
	}

	assertMessageOffset(t, <-consumer.Messages(), 1)
	assertMessageOffset(t, <-consumer.Messages(), 2)

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()
}

func TestConsumeMessageWithSessionIDs(t *testing.T) {
	// Given
	fetchResponse1 := &FetchResponse{Version: 7}
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 1)
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 2)

	cfg := NewTestConfig()
	cfg.Version = V1_1_0_0

	broker0 := NewMockBroker(t, 0)
	fetchResponse2 := &FetchResponse{}
	fetchResponse2.Version = 7
	fetchResponse2.AddError("my_topic", 0, ErrNoError)

	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetVersion(1).
			SetOffset("my_topic", 0, OffsetNewest, 1234).
			SetOffset("my_topic", 0, OffsetOldest, 0),
		"FetchRequest": NewMockSequence(fetchResponse1, fetchResponse2),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// When
	consumer, err := master.ConsumePartition("my_topic", 0, 1)
	if err != nil {
		t.Fatal(err)
	}

	assertMessageOffset(t, <-consumer.Messages(), 1)
	assertMessageOffset(t, <-consumer.Messages(), 2)

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()

	fetchReq := broker0.History()[3].Request.(*FetchRequest)
	if fetchReq.SessionID != 0 || fetchReq.SessionEpoch != -1 {
		t.Error("Expected session ID to be zero & Epoch to be -1")
	}
}

func TestConsumeMessagesFromReadReplica(t *testing.T) {
	// Given
	fetchResponse1 := &FetchResponse{Version: 11}
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 1)
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 2)
	block1 := fetchResponse1.GetBlock("my_topic", 0)
	block1.PreferredReadReplica = -1

	fetchResponse2 := &FetchResponse{Version: 11}
	// Create a block with no records.
	block2 := fetchResponse1.getOrCreateBlock("my_topic", 0)
	block2.PreferredReadReplica = 1

	fetchResponse3 := &FetchResponse{Version: 11}
	fetchResponse3.AddMessage("my_topic", 0, nil, testMsg, 3)
	fetchResponse3.AddMessage("my_topic", 0, nil, testMsg, 4)
	block3 := fetchResponse3.GetBlock("my_topic", 0)
	block3.PreferredReadReplica = -1

	fetchResponse4 := &FetchResponse{Version: 11}
	fetchResponse4.AddMessage("my_topic", 0, nil, testMsg, 5)
	fetchResponse4.AddMessage("my_topic", 0, nil, testMsg, 6)
	block4 := fetchResponse4.GetBlock("my_topic", 0)
	block4.PreferredReadReplica = -1

	cfg := NewConfig()
	cfg.Version = V2_3_0_0
	cfg.RackID = "consumer_rack"

	leader := NewMockBroker(t, 0)
	broker0 := NewMockBroker(t, 1)

	leader.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetBroker(leader.Addr(), leader.BrokerID()).
			SetLeader("my_topic", 0, leader.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetVersion(1).
			SetOffset("my_topic", 0, OffsetNewest, 1234).
			SetOffset("my_topic", 0, OffsetOldest, 0),
		"FetchRequest": NewMockSequence(fetchResponse1, fetchResponse2),
	})

	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetBroker(leader.Addr(), leader.BrokerID()).
			SetLeader("my_topic", 0, leader.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetVersion(1).
			SetOffset("my_topic", 0, OffsetNewest, 1234).
			SetOffset("my_topic", 0, OffsetOldest, 0),
		"FetchRequest": NewMockSequence(fetchResponse3, fetchResponse4),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// When
	consumer, err := master.ConsumePartition("my_topic", 0, 1)
	if err != nil {
		t.Fatal(err)
	}

	assertMessageOffset(t, <-consumer.Messages(), 1)
	assertMessageOffset(t, <-consumer.Messages(), 2)
	assertMessageOffset(t, <-consumer.Messages(), 3)
	assertMessageOffset(t, <-consumer.Messages(), 4)

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()
	leader.Close()
}

func TestConsumeMessagesFromReadReplicaLeaderFallback(t *testing.T) {
	// Given
	fetchResponse1 := &FetchResponse{Version: 11}
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 1)
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 2)
	block1 := fetchResponse1.GetBlock("my_topic", 0)
	block1.PreferredReadReplica = 5 // Does not exist.

	fetchResponse2 := &FetchResponse{Version: 11}
	fetchResponse2.AddMessage("my_topic", 0, nil, testMsg, 3)
	fetchResponse2.AddMessage("my_topic", 0, nil, testMsg, 4)
	block2 := fetchResponse2.GetBlock("my_topic", 0)
	block2.PreferredReadReplica = -1

	cfg := NewConfig()
	cfg.Version = V2_3_0_0
	cfg.RackID = "consumer_rack"

	leader := NewMockBroker(t, 0)

	leader.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(leader.Addr(), leader.BrokerID()).
			SetLeader("my_topic", 0, leader.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetVersion(1).
			SetOffset("my_topic", 0, OffsetNewest, 1234).
			SetOffset("my_topic", 0, OffsetOldest, 0),
		"FetchRequest": NewMockSequence(fetchResponse1, fetchResponse2),
	})

	master, err := NewConsumer([]string{leader.Addr()}, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// When
	consumer, err := master.ConsumePartition("my_topic", 0, 1)
	if err != nil {
		t.Fatal(err)
	}

	assertMessageOffset(t, <-consumer.Messages(), 1)
	assertMessageOffset(t, <-consumer.Messages(), 2)
	assertMessageOffset(t, <-consumer.Messages(), 3)
	assertMessageOffset(t, <-consumer.Messages(), 4)

	safeClose(t, consumer)
	safeClose(t, master)
	leader.Close()
}

func TestConsumeMessagesFromReadReplicaErrorReplicaNotAvailable(t *testing.T) {
	// Given
	fetchResponse1 := &FetchResponse{Version: 11}
	block1 := fetchResponse1.getOrCreateBlock("my_topic", 0)
	block1.PreferredReadReplica = 1

	fetchResponse2 := &FetchResponse{Version: 11}
	fetchResponse2.AddMessage("my_topic", 0, nil, testMsg, 1)
	fetchResponse2.AddMessage("my_topic", 0, nil, testMsg, 2)
	block2 := fetchResponse2.GetBlock("my_topic", 0)
	block2.PreferredReadReplica = -1

	fetchResponse3 := &FetchResponse{Version: 11}
	fetchResponse3.AddError("my_topic", 0, ErrReplicaNotAvailable)

	fetchResponse4 := &FetchResponse{Version: 11}
	fetchResponse4.AddMessage("my_topic", 0, nil, testMsg, 3)
	fetchResponse4.AddMessage("my_topic", 0, nil, testMsg, 4)

	cfg := NewConfig()
	cfg.Version = V2_3_0_0
	cfg.RackID = "consumer_rack"

	leader := NewMockBroker(t, 0)
	broker0 := NewMockBroker(t, 1)

	leader.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetBroker(leader.Addr(), leader.BrokerID()).
			SetLeader("my_topic", 0, leader.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetVersion(1).
			SetOffset("my_topic", 0, OffsetNewest, 1234).
			SetOffset("my_topic", 0, OffsetOldest, 0),
		"FetchRequest": NewMockSequence(fetchResponse1, fetchResponse4),
	})

	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetBroker(leader.Addr(), leader.BrokerID()).
			SetLeader("my_topic", 0, leader.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetVersion(1).
			SetOffset("my_topic", 0, OffsetNewest, 1234).
			SetOffset("my_topic", 0, OffsetOldest, 0),
		"FetchRequest": NewMockSequence(fetchResponse2, fetchResponse3),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// When
	consumer, err := master.ConsumePartition("my_topic", 0, 1)
	if err != nil {
		t.Fatal(err)
	}

	assertMessageOffset(t, <-consumer.Messages(), 1)
	assertMessageOffset(t, <-consumer.Messages(), 2)
	assertMessageOffset(t, <-consumer.Messages(), 3)
	assertMessageOffset(t, <-consumer.Messages(), 4)

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()
	leader.Close()
}

func TestConsumeMessagesFromReadReplicaErrorUnknown(t *testing.T) {
	// Given
	fetchResponse1 := &FetchResponse{Version: 11}
	block1 := fetchResponse1.getOrCreateBlock("my_topic", 0)
	block1.PreferredReadReplica = 1

	fetchResponse2 := &FetchResponse{Version: 11}
	fetchResponse2.AddMessage("my_topic", 0, nil, testMsg, 1)
	fetchResponse2.AddMessage("my_topic", 0, nil, testMsg, 2)
	block2 := fetchResponse2.GetBlock("my_topic", 0)
	block2.PreferredReadReplica = -1

	fetchResponse3 := &FetchResponse{Version: 11}
	fetchResponse3.AddError("my_topic", 0, ErrUnknown)

	fetchResponse4 := &FetchResponse{Version: 11}
	fetchResponse4.AddMessage("my_topic", 0, nil, testMsg, 3)
	fetchResponse4.AddMessage("my_topic", 0, nil, testMsg, 4)

	cfg := NewConfig()
	cfg.Version = V2_3_0_0
	cfg.RackID = "consumer_rack"

	leader := NewMockBroker(t, 0)
	broker0 := NewMockBroker(t, 1)

	leader.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetBroker(leader.Addr(), leader.BrokerID()).
			SetLeader("my_topic", 0, leader.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetVersion(1).
			SetOffset("my_topic", 0, OffsetNewest, 1234).
			SetOffset("my_topic", 0, OffsetOldest, 0),
		"FetchRequest": NewMockSequence(fetchResponse1, fetchResponse4),
	})

	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetBroker(leader.Addr(), leader.BrokerID()).
			SetLeader("my_topic", 0, leader.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetVersion(1).
			SetOffset("my_topic", 0, OffsetNewest, 1234).
			SetOffset("my_topic", 0, OffsetOldest, 0),
		"FetchRequest": NewMockSequence(fetchResponse2, fetchResponse3),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// When
	consumer, err := master.ConsumePartition("my_topic", 0, 1)
	if err != nil {
		t.Fatal(err)
	}

	assertMessageOffset(t, <-consumer.Messages(), 1)
	assertMessageOffset(t, <-consumer.Messages(), 2)
	assertMessageOffset(t, <-consumer.Messages(), 3)
	assertMessageOffset(t, <-consumer.Messages(), 4)

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()
	leader.Close()
}

// TestConsumeMessagesTrackLeader ensures that in the event that leadership of
// a topicPartition changes and no preferredReadReplica is specified, the
// consumer connects back to the new leader to resume consumption and doesn't
// continue consuming from the follower.
//
// See https://github.com/Shopify/sarama/issues/1927
func TestConsumeMessagesTrackLeader(t *testing.T) {
	cfg := NewConfig()
	cfg.ClientID = t.Name()
	cfg.Metadata.RefreshFrequency = time.Millisecond * 50
	cfg.Net.MaxOpenRequests = 1
	cfg.Version = V2_1_0_0

	leader1 := NewMockBroker(t, 1)
	leader2 := NewMockBroker(t, 2)

	mockMetadataResponse1 := NewMockMetadataResponse(t).
		SetBroker(leader1.Addr(), leader1.BrokerID()).
		SetBroker(leader2.Addr(), leader2.BrokerID()).
		SetLeader("my_topic", 0, leader1.BrokerID())
	mockMetadataResponse2 := NewMockMetadataResponse(t).
		SetBroker(leader1.Addr(), leader1.BrokerID()).
		SetBroker(leader2.Addr(), leader2.BrokerID()).
		SetLeader("my_topic", 0, leader2.BrokerID())
	mockMetadataResponse3 := NewMockMetadataResponse(t).
		SetBroker(leader1.Addr(), leader1.BrokerID()).
		SetBroker(leader2.Addr(), leader2.BrokerID()).
		SetLeader("my_topic", 0, leader1.BrokerID())

	leader1.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": mockMetadataResponse1,
		"OffsetRequest": NewMockOffsetResponse(t).
			SetVersion(1).
			SetOffset("my_topic", 0, OffsetNewest, 1234).
			SetOffset("my_topic", 0, OffsetOldest, 0),
		"FetchRequest": NewMockFetchResponse(t, 1).
			SetVersion(10).
			SetMessage("my_topic", 0, 1, testMsg).
			SetMessage("my_topic", 0, 2, testMsg),
	})

	client, err := NewClient([]string{leader1.Addr()}, cfg)
	if err != nil {
		t.Fatal(err)
	}

	consumer, err := NewConsumerFromClient(client)
	if err != nil {
		t.Fatal(err)
	}

	pConsumer, err := consumer.ConsumePartition("my_topic", 0, 1)
	if err != nil {
		t.Fatal(err)
	}

	assertMessageOffset(t, <-pConsumer.Messages(), 1)
	assertMessageOffset(t, <-pConsumer.Messages(), 2)

	fetchEmptyResponse := &FetchResponse{Version: 10}
	fetchEmptyResponse.AddError("my_topic", 0, ErrNoError)
	leader1.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": mockMetadataResponse2,
		"FetchRequest":    NewMockWrapper(fetchEmptyResponse),
	})
	leader2.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": mockMetadataResponse2,
		"FetchRequest": NewMockFetchResponse(t, 1).
			SetVersion(10).
			SetMessage("my_topic", 0, 3, testMsg).
			SetMessage("my_topic", 0, 4, testMsg),
	})

	// wait for client to be aware that leadership has changed
	for {
		b, _ := client.Leader("my_topic", 0)
		if b.ID() == int32(2) {
			break
		}
		time.Sleep(time.Millisecond * 50)
	}

	assertMessageOffset(t, <-pConsumer.Messages(), 3)
	assertMessageOffset(t, <-pConsumer.Messages(), 4)

	leader1.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": mockMetadataResponse3,
		"FetchRequest": NewMockFetchResponse(t, 1).
			SetVersion(10).
			SetMessage("my_topic", 0, 5, testMsg).
			SetMessage("my_topic", 0, 6, testMsg),
	})
	leader2.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": mockMetadataResponse3,
		"FetchRequest":    NewMockWrapper(fetchEmptyResponse),
	})

	// wait for client to be aware that leadership has changed back again
	for {
		b, _ := client.Leader("my_topic", 0)
		if b.ID() == int32(1) {
			break
		}
		time.Sleep(time.Millisecond * 50)
	}

	assertMessageOffset(t, <-pConsumer.Messages(), 5)
	assertMessageOffset(t, <-pConsumer.Messages(), 6)

	safeClose(t, pConsumer)
	safeClose(t, consumer)
	leader1.Close()
	leader2.Close()
}

// It is fine if offsets of fetched messages are not sequential (although
// strictly increasing!).
func TestConsumerNonSequentialOffsets(t *testing.T) {
	// Given
	legacyFetchResponse := &FetchResponse{}
	legacyFetchResponse.AddMessage("my_topic", 0, nil, testMsg, 5)
	legacyFetchResponse.AddMessage("my_topic", 0, nil, testMsg, 7)
	legacyFetchResponse.AddMessage("my_topic", 0, nil, testMsg, 11)
	newFetchResponse := &FetchResponse{Version: 4}
	newFetchResponse.AddRecord("my_topic", 0, nil, testMsg, 5)
	newFetchResponse.AddRecord("my_topic", 0, nil, testMsg, 7)
	newFetchResponse.AddRecord("my_topic", 0, nil, testMsg, 11)
	newFetchResponse.SetLastOffsetDelta("my_topic", 0, 11)
	newFetchResponse.SetLastStableOffset("my_topic", 0, 11)
	for _, fetchResponse1 := range []*FetchResponse{legacyFetchResponse, newFetchResponse} {
		var offsetResponseVersion int16
		cfg := NewTestConfig()
		if fetchResponse1.Version >= 4 {
			cfg.Version = V0_11_0_0
			offsetResponseVersion = 1
		}

		broker0 := NewMockBroker(t, 0)
		fetchResponse2 := &FetchResponse{Version: fetchResponse1.Version}
		fetchResponse2.AddError("my_topic", 0, ErrNoError)
		broker0.SetHandlerByMap(map[string]MockResponse{
			"MetadataRequest": NewMockMetadataResponse(t).
				SetBroker(broker0.Addr(), broker0.BrokerID()).
				SetLeader("my_topic", 0, broker0.BrokerID()),
			"OffsetRequest": NewMockOffsetResponse(t).
				SetVersion(offsetResponseVersion).
				SetOffset("my_topic", 0, OffsetNewest, 1234).
				SetOffset("my_topic", 0, OffsetOldest, 0),
			"FetchRequest": NewMockSequence(fetchResponse1, fetchResponse2),
		})

		master, err := NewConsumer([]string{broker0.Addr()}, cfg)
		if err != nil {
			t.Fatal(err)
		}

		// When
		consumer, err := master.ConsumePartition("my_topic", 0, 3)
		if err != nil {
			t.Fatal(err)
		}

		// Then: messages with offsets 1 and 2 are not returned even though they
		// are present in the response.
		assertMessageOffset(t, <-consumer.Messages(), 5)
		assertMessageOffset(t, <-consumer.Messages(), 7)
		assertMessageOffset(t, <-consumer.Messages(), 11)

		safeClose(t, consumer)
		safeClose(t, master)
		broker0.Close()
	}
}

// If leadership for a partition is changing then consumer resolves the new
// leader and switches to it.
func TestConsumerRebalancingMultiplePartitions(t *testing.T) {
	// initial setup
	seedBroker := NewMockBroker(t, 10)
	leader0 := NewMockBroker(t, 0)
	leader1 := NewMockBroker(t, 1)

	seedBroker.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(leader0.Addr(), leader0.BrokerID()).
			SetBroker(leader1.Addr(), leader1.BrokerID()).
			SetBroker(seedBroker.Addr(), seedBroker.BrokerID()).
			SetLeader("my_topic", 0, leader0.BrokerID()).
			SetLeader("my_topic", 1, leader1.BrokerID()),
	})

	mockOffsetResponse1 := NewMockOffsetResponse(t).
		SetOffset("my_topic", 0, OffsetOldest, 0).
		SetOffset("my_topic", 0, OffsetNewest, 1000).
		SetOffset("my_topic", 1, OffsetOldest, 0).
		SetOffset("my_topic", 1, OffsetNewest, 1000)
	leader0.SetHandlerByMap(map[string]MockResponse{
		"OffsetRequest": mockOffsetResponse1,
		"FetchRequest":  NewMockFetchResponse(t, 1),
	})
	leader1.SetHandlerByMap(map[string]MockResponse{
		"OffsetRequest": mockOffsetResponse1,
		"FetchRequest":  NewMockFetchResponse(t, 1),
	})

	// launch test goroutines
	config := NewTestConfig()
	config.Consumer.Retry.Backoff = 50
	master, err := NewConsumer([]string{seedBroker.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	// we expect to end up (eventually) consuming exactly ten messages on each partition
	var wg sync.WaitGroup
	for i := int32(0); i < 2; i++ {
		consumer, err := master.ConsumePartition("my_topic", i, 0)
		if err != nil {
			t.Error(err)
		}

		go func(c PartitionConsumer) {
			for err := range c.Errors() {
				t.Error(err)
			}
		}(consumer)

		wg.Add(1)
		go func(partition int32, c PartitionConsumer) {
			for i := 0; i < 10; i++ {
				message := <-consumer.Messages()
				if message.Offset != int64(i) {
					t.Error("Incorrect message offset!", i, partition, message.Offset)
				}
				if message.Partition != partition {
					t.Error("Incorrect message partition!")
				}
			}
			safeClose(t, consumer)
			wg.Done()
		}(i, consumer)
	}

	time.Sleep(50 * time.Millisecond)
	Logger.Printf("    STAGE 1")
	// Stage 1:
	//   * my_topic/0 -> leader0 serves 4 messages
	//   * my_topic/1 -> leader1 serves 0 messages

	mockFetchResponse := NewMockFetchResponse(t, 1)
	for i := 0; i < 4; i++ {
		mockFetchResponse.SetMessage("my_topic", 0, int64(i), testMsg)
	}
	leader0.SetHandlerByMap(map[string]MockResponse{
		"FetchRequest": mockFetchResponse,
	})

	time.Sleep(50 * time.Millisecond)
	Logger.Printf("    STAGE 2")
	// Stage 2:
	//   * leader0 says that it is no longer serving my_topic/0
	//   * seedBroker tells that leader1 is serving my_topic/0 now

	// seed broker tells that the new partition 0 leader is leader1
	seedBroker.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetLeader("my_topic", 0, leader1.BrokerID()).
			SetLeader("my_topic", 1, leader1.BrokerID()).
			SetBroker(leader0.Addr(), leader0.BrokerID()).
			SetBroker(leader1.Addr(), leader1.BrokerID()).
			SetBroker(seedBroker.Addr(), seedBroker.BrokerID()),
	})

	// leader0 says no longer leader of partition 0
	fetchResponse := new(FetchResponse)
	fetchResponse.AddError("my_topic", 0, ErrNotLeaderForPartition)
	leader0.SetHandlerByMap(map[string]MockResponse{
		"FetchRequest": NewMockWrapper(fetchResponse),
	})

	time.Sleep(50 * time.Millisecond)
	Logger.Printf("    STAGE 3")
	// Stage 3:
	//   * my_topic/0 -> leader1 serves 3 messages
	//   * my_topic/1 -> leader1 server 8 messages

	// leader1 provides 3 message on partition 0, and 8 messages on partition 1
	mockFetchResponse2 := NewMockFetchResponse(t, 2)
	for i := 4; i < 7; i++ {
		mockFetchResponse2.SetMessage("my_topic", 0, int64(i), testMsg)
	}
	for i := 0; i < 8; i++ {
		mockFetchResponse2.SetMessage("my_topic", 1, int64(i), testMsg)
	}
	leader1.SetHandlerByMap(map[string]MockResponse{
		"FetchRequest": mockFetchResponse2,
	})

	time.Sleep(50 * time.Millisecond)
	Logger.Printf("    STAGE 4")
	// Stage 4:
	//   * my_topic/0 -> leader1 serves 3 messages
	//   * my_topic/1 -> leader1 tells that it is no longer the leader
	//   * seedBroker tells that leader0 is a new leader for my_topic/1

	// metadata assigns 0 to leader1 and 1 to leader0
	seedBroker.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetLeader("my_topic", 0, leader1.BrokerID()).
			SetLeader("my_topic", 1, leader0.BrokerID()).
			SetBroker(leader0.Addr(), leader0.BrokerID()).
			SetBroker(leader1.Addr(), leader1.BrokerID()).
			SetBroker(seedBroker.Addr(), seedBroker.BrokerID()),
	})

	// leader1 provides three more messages on partition0, says no longer leader of partition1
	mockFetchResponse3 := NewMockFetchResponse(t, 3).
		SetMessage("my_topic", 0, int64(7), testMsg).
		SetMessage("my_topic", 0, int64(8), testMsg).
		SetMessage("my_topic", 0, int64(9), testMsg)
	fetchResponse4 := new(FetchResponse)
	fetchResponse4.AddError("my_topic", 1, ErrNotLeaderForPartition)
	leader1.SetHandlerByMap(map[string]MockResponse{
		"FetchRequest": NewMockSequence(mockFetchResponse3, fetchResponse4),
	})

	// leader0 provides two messages on partition 1
	mockFetchResponse4 := NewMockFetchResponse(t, 2)
	for i := 8; i < 10; i++ {
		mockFetchResponse4.SetMessage("my_topic", 1, int64(i), testMsg)
	}
	leader0.SetHandlerByMap(map[string]MockResponse{
		"FetchRequest": mockFetchResponse4,
	})

	wg.Wait()
	safeClose(t, master)
	leader1.Close()
	leader0.Close()
	seedBroker.Close()
}

// When two partitions have the same broker as the leader, if one partition
// consumer channel buffer is full then that does not affect the ability to
// read messages by the other consumer.
func TestConsumerInterleavedClose(t *testing.T) {
	// Given
	broker0 := NewMockBroker(t, 0)
	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()).
			SetLeader("my_topic", 1, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, OffsetOldest, 1000).
			SetOffset("my_topic", 0, OffsetNewest, 1100).
			SetOffset("my_topic", 1, OffsetOldest, 2000).
			SetOffset("my_topic", 1, OffsetNewest, 2100),
		"FetchRequest": NewMockFetchResponse(t, 1).
			SetMessage("my_topic", 0, 1000, testMsg).
			SetMessage("my_topic", 0, 1001, testMsg).
			SetMessage("my_topic", 0, 1002, testMsg).
			SetMessage("my_topic", 1, 2000, testMsg),
	})

	config := NewTestConfig()
	config.ChannelBufferSize = 0
	master, err := NewConsumer([]string{broker0.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	c0, err := master.ConsumePartition("my_topic", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	c1, err := master.ConsumePartition("my_topic", 1, 2000)
	if err != nil {
		t.Fatal(err)
	}

	// When/Then: we can read from partition 0 even if nobody reads from partition 1
	assertMessageOffset(t, <-c0.Messages(), 1000)
	assertMessageOffset(t, <-c0.Messages(), 1001)
	assertMessageOffset(t, <-c0.Messages(), 1002)

	safeClose(t, c1)
	safeClose(t, c0)
	safeClose(t, master)
	broker0.Close()
}

func TestConsumerBounceWithReferenceOpen(t *testing.T) {
	broker0 := NewMockBroker(t, 0)
	broker0Addr := broker0.Addr()
	broker1 := NewMockBroker(t, 1)

	mockMetadataResponse := NewMockMetadataResponse(t).
		SetBroker(broker0.Addr(), broker0.BrokerID()).
		SetBroker(broker1.Addr(), broker1.BrokerID()).
		SetLeader("my_topic", 0, broker0.BrokerID()).
		SetLeader("my_topic", 1, broker1.BrokerID())

	mockOffsetResponse := NewMockOffsetResponse(t).
		SetOffset("my_topic", 0, OffsetOldest, 1000).
		SetOffset("my_topic", 0, OffsetNewest, 1100).
		SetOffset("my_topic", 1, OffsetOldest, 2000).
		SetOffset("my_topic", 1, OffsetNewest, 2100)

	mockFetchResponse := NewMockFetchResponse(t, 1)
	for i := 0; i < 10; i++ {
		mockFetchResponse.SetMessage("my_topic", 0, int64(1000+i), testMsg)
		mockFetchResponse.SetMessage("my_topic", 1, int64(2000+i), testMsg)
	}

	broker0.SetHandlerByMap(map[string]MockResponse{
		"OffsetRequest": mockOffsetResponse,
		"FetchRequest":  mockFetchResponse,
	})
	broker1.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": mockMetadataResponse,
		"OffsetRequest":   mockOffsetResponse,
		"FetchRequest":    mockFetchResponse,
	})

	config := NewTestConfig()
	config.Consumer.Return.Errors = true
	config.Consumer.Retry.Backoff = 100 * time.Millisecond
	config.ChannelBufferSize = 1
	master, err := NewConsumer([]string{broker1.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	c0, err := master.ConsumePartition("my_topic", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	c1, err := master.ConsumePartition("my_topic", 1, 2000)
	if err != nil {
		t.Fatal(err)
	}

	// read messages from both partition to make sure that both brokers operate
	// normally.
	assertMessageOffset(t, <-c0.Messages(), 1000)
	assertMessageOffset(t, <-c1.Messages(), 2000)

	// Simulate broker shutdown. Note that metadata response does not change,
	// that is the leadership does not move to another broker. So partition
	// consumer will keep retrying to restore the connection with the broker.
	broker0.Close()

	// Make sure that while the partition/0 leader is down, consumer/partition/1
	// is capable of pulling messages from broker1.
	for i := 1; i < 7; i++ {
		offset := (<-c1.Messages()).Offset
		if offset != int64(2000+i) {
			t.Errorf("Expected offset %d from consumer/partition/1", int64(2000+i))
		}
	}

	// Bring broker0 back to service.
	broker0 = NewMockBrokerAddr(t, 0, broker0Addr)
	broker0.SetHandlerByMap(map[string]MockResponse{
		"FetchRequest": mockFetchResponse,
	})

	// Read the rest of messages from both partitions.
	for i := 7; i < 10; i++ {
		assertMessageOffset(t, <-c1.Messages(), int64(2000+i))
	}
	for i := 1; i < 10; i++ {
		assertMessageOffset(t, <-c0.Messages(), int64(1000+i))
	}

	select {
	case <-c0.Errors():
	default:
		t.Errorf("Partition consumer should have detected broker restart")
	}

	safeClose(t, c1)
	safeClose(t, c0)
	safeClose(t, master)
	broker0.Close()
	broker1.Close()
}

func TestConsumerOffsetOutOfRange(t *testing.T) {
	// Given
	broker0 := NewMockBroker(t, 2)
	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, OffsetNewest, 1234).
			SetOffset("my_topic", 0, OffsetOldest, 2345),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, NewTestConfig())
	if err != nil {
		t.Fatal(err)
	}

	// When/Then
	if _, err := master.ConsumePartition("my_topic", 0, 0); err != ErrOffsetOutOfRange {
		t.Fatal("Should return ErrOffsetOutOfRange, got:", err)
	}
	if _, err := master.ConsumePartition("my_topic", 0, 3456); err != ErrOffsetOutOfRange {
		t.Fatal("Should return ErrOffsetOutOfRange, got:", err)
	}
	if _, err := master.ConsumePartition("my_topic", 0, -3); err != ErrOffsetOutOfRange {
		t.Fatal("Should return ErrOffsetOutOfRange, got:", err)
	}

	safeClose(t, master)
	broker0.Close()
}

func TestConsumerExpiryTicker(t *testing.T) {
	// Given
	broker0 := NewMockBroker(t, 0)
	fetchResponse1 := &FetchResponse{}
	for i := 1; i <= 8; i++ {
		fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, int64(i))
	}
	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, OffsetNewest, 1234).
			SetOffset("my_topic", 0, OffsetOldest, 1),
		"FetchRequest": NewMockSequence(fetchResponse1),
	})

	config := NewTestConfig()
	config.ChannelBufferSize = 0
	config.Consumer.MaxProcessingTime = 10 * time.Millisecond
	master, err := NewConsumer([]string{broker0.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	// When
	consumer, err := master.ConsumePartition("my_topic", 0, 1)
	if err != nil {
		t.Fatal(err)
	}

	// Then: messages with offsets 1 through 8 are read
	for i := 1; i <= 8; i++ {
		assertMessageOffset(t, <-consumer.Messages(), int64(i))
		time.Sleep(2 * time.Millisecond)
	}

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()
}

func TestConsumerTimestamps(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	type testMessage struct {
		key       Encoder
		offset    int64
		timestamp time.Time
	}
	for _, d := range []struct {
		kversion          KafkaVersion
		logAppendTime     bool
		messages          []testMessage
		expectedTimestamp []time.Time
	}{
		{MinVersion, false, []testMessage{
			{testMsg, 1, now},
			{testMsg, 2, now},
		}, []time.Time{{}, {}}},
		{V0_9_0_0, false, []testMessage{
			{testMsg, 1, now},
			{testMsg, 2, now},
		}, []time.Time{{}, {}}},
		{V0_10_0_0, false, []testMessage{
			{testMsg, 1, now},
			{testMsg, 2, now},
		}, []time.Time{{}, {}}},
		{V0_10_2_1, false, []testMessage{
			{testMsg, 1, now.Add(time.Second)},
			{testMsg, 2, now.Add(2 * time.Second)},
		}, []time.Time{now.Add(time.Second), now.Add(2 * time.Second)}},
		{V0_10_2_1, true, []testMessage{
			{testMsg, 1, now.Add(time.Second)},
			{testMsg, 2, now.Add(2 * time.Second)},
		}, []time.Time{now, now}},
		{V0_11_0_0, false, []testMessage{
			{testMsg, 1, now.Add(time.Second)},
			{testMsg, 2, now.Add(2 * time.Second)},
		}, []time.Time{now.Add(time.Second), now.Add(2 * time.Second)}},
		{V0_11_0_0, true, []testMessage{
			{testMsg, 1, now.Add(time.Second)},
			{testMsg, 2, now.Add(2 * time.Second)},
		}, []time.Time{now, now}},
	} {
		var fr *FetchResponse
		var offsetResponseVersion int16
		cfg := NewTestConfig()
		cfg.Version = d.kversion
		switch {
		case d.kversion.IsAtLeast(V0_11_0_0):
			offsetResponseVersion = 1
			fr = &FetchResponse{Version: 4, LogAppendTime: d.logAppendTime, Timestamp: now}
			for _, m := range d.messages {
				fr.AddRecordWithTimestamp("my_topic", 0, m.key, testMsg, m.offset, m.timestamp)
			}
			fr.SetLastOffsetDelta("my_topic", 0, 2)
			fr.SetLastStableOffset("my_topic", 0, 2)
		case d.kversion.IsAtLeast(V0_10_1_0):
			offsetResponseVersion = 1
			fr = &FetchResponse{Version: 3, LogAppendTime: d.logAppendTime, Timestamp: now}
			for _, m := range d.messages {
				fr.AddMessageWithTimestamp("my_topic", 0, m.key, testMsg, m.offset, m.timestamp, 1)
			}
		default:
			var version int16
			switch {
			case d.kversion.IsAtLeast(V0_10_0_0):
				version = 2
			case d.kversion.IsAtLeast(V0_9_0_0):
				version = 1
			}
			fr = &FetchResponse{Version: version}
			for _, m := range d.messages {
				fr.AddMessageWithTimestamp("my_topic", 0, m.key, testMsg, m.offset, m.timestamp, 0)
			}
		}

		broker0 := NewMockBroker(t, 0)
		broker0.SetHandlerByMap(map[string]MockResponse{
			"MetadataRequest": NewMockMetadataResponse(t).
				SetBroker(broker0.Addr(), broker0.BrokerID()).
				SetLeader("my_topic", 0, broker0.BrokerID()),
			"OffsetRequest": NewMockOffsetResponse(t).
				SetVersion(offsetResponseVersion).
				SetOffset("my_topic", 0, OffsetNewest, 1234).
				SetOffset("my_topic", 0, OffsetOldest, 0),
			"FetchRequest": NewMockSequence(fr),
		})

		master, err := NewConsumer([]string{broker0.Addr()}, cfg)
		if err != nil {
			t.Fatal(err)
		}

		consumer, err := master.ConsumePartition("my_topic", 0, 1)
		if err != nil {
			t.Fatal(err)
		}

		for i, ts := range d.expectedTimestamp {
			select {
			case msg := <-consumer.Messages():
				assertMessageOffset(t, msg, int64(i)+1)
				if !msg.Timestamp.Equal(ts) {
					t.Errorf("Wrong timestamp (kversion:%v, logAppendTime:%v): got: %v, want: %v",
						d.kversion, d.logAppendTime, msg.Timestamp, ts)
				}
			case err := <-consumer.Errors():
				t.Fatal(err)
			}
		}

		safeClose(t, consumer)
		safeClose(t, master)
		broker0.Close()
	}
}

// When set to ReadCommitted, no uncommitted message should be available in messages channel
func TestExcludeUncommitted(t *testing.T) {
	// Given
	broker0 := NewMockBroker(t, 0)

	fetchResponse := &FetchResponse{
		Version: 4,
		Blocks: map[string]map[int32]*FetchResponseBlock{"my_topic": {0: {
			AbortedTransactions: []*AbortedTransaction{{ProducerID: 7, FirstOffset: 1235}},
		}}},
	}
	fetchResponse.AddRecordBatch("my_topic", 0, nil, testMsg, 1234, 7, true)   // committed msg
	fetchResponse.AddRecordBatch("my_topic", 0, nil, testMsg, 1235, 7, true)   // uncommitted msg
	fetchResponse.AddRecordBatch("my_topic", 0, nil, testMsg, 1236, 7, true)   // uncommitted msg
	fetchResponse.AddControlRecord("my_topic", 0, 1237, 7, ControlRecordAbort) // abort control record
	fetchResponse.AddRecordBatch("my_topic", 0, nil, testMsg, 1238, 7, true)   // committed msg

	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetVersion(1).
			SetOffset("my_topic", 0, OffsetOldest, 0).
			SetOffset("my_topic", 0, OffsetNewest, 1237),
		"FetchRequest": NewMockWrapper(fetchResponse),
	})

	cfg := NewTestConfig()
	cfg.Consumer.Return.Errors = true
	cfg.Version = V0_11_0_0
	cfg.Consumer.IsolationLevel = ReadCommitted

	// When
	master, err := NewConsumer([]string{broker0.Addr()}, cfg)
	if err != nil {
		t.Fatal(err)
	}

	consumer, err := master.ConsumePartition("my_topic", 0, 1234)
	if err != nil {
		t.Fatal(err)
	}

	// Then: only the 2 committed messages are returned
	select {
	case message := <-consumer.Messages():
		assertMessageOffset(t, message, int64(1234))
	case err := <-consumer.Errors():
		t.Error(err)
	}
	select {
	case message := <-consumer.Messages():
		assertMessageOffset(t, message, int64(1238))
	case err := <-consumer.Errors():
		t.Error(err)
	}

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()
}

func assertMessageOffset(t *testing.T, msg *ConsumerMessage, expectedOffset int64) {
	t.Helper()
	if msg.Offset != expectedOffset {
		t.Fatalf("Incorrect message offset: expected=%d, actual=%d", expectedOffset, msg.Offset)
	}
}

// This example shows how to use the consumer to read messages
// from a single partition.
func ExampleConsumer() {
	consumer, err := NewConsumer([]string{"localhost:9092"}, NewTestConfig())
	if err != nil {
		panic(err)
	}

	defer func() {
		if err := consumer.Close(); err != nil {
			log.Fatalln(err)
		}
	}()

	partitionConsumer, err := consumer.ConsumePartition("my_topic", 0, OffsetNewest)
	if err != nil {
		panic(err)
	}

	defer func() {
		if err := partitionConsumer.Close(); err != nil {
			log.Fatalln(err)
		}
	}()

	// Trap SIGINT to trigger a shutdown.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)

	consumed := 0
ConsumerLoop:
	for {
		select {
		case msg := <-partitionConsumer.Messages():
			log.Printf("Consumed message offset %d\n", msg.Offset)
			consumed++
		case <-signals:
			break ConsumerLoop
		}
	}

	log.Printf("Consumed: %d\n", consumed)
}

func Test_partitionConsumer_parseResponse(t *testing.T) {
	type args struct {
		response *FetchResponse
	}
	tests := []struct {
		name    string
		args    args
		want    []*ConsumerMessage
		wantErr bool
	}{
		{
			name: "empty but throttled FetchResponse is not considered an error",
			args: args{
				response: &FetchResponse{
					ThrottleTime: time.Millisecond,
				},
			},
		},
		{
			name: "empty FetchResponse is considered an incomplete response by default",
			args: args{
				response: &FetchResponse{},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			child := &partitionConsumer{
				broker: &brokerConsumer{
					broker: &Broker{},
				},
				conf: &Config{},
			}
			got, err := child.parseResponse(tt.args.response)
			if (err != nil) != tt.wantErr {
				t.Errorf("partitionConsumer.parseResponse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("partitionConsumer.parseResponse() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_partitionConsumer_parseResponseEmptyBatch(t *testing.T) {
	lrbOffset := int64(5)
	block := &FetchResponseBlock{
		HighWaterMarkOffset:    10,
		LastStableOffset:       10,
		LastRecordsBatchOffset: &lrbOffset,
		LogStartOffset:         0,
	}
	response := &FetchResponse{
		Blocks:  map[string]map[int32]*FetchResponseBlock{"my_topic": {0: block}},
		Version: 2,
	}
	child := &partitionConsumer{
		broker: &brokerConsumer{
			broker: &Broker{},
		},
		conf:      NewConfig(),
		topic:     "my_topic",
		partition: 0,
	}
	got, err := child.parseResponse(response)
	if err != nil {
		t.Errorf("partitionConsumer.parseResponse() error = %v", err)
		return
	}
	if got != nil {
		t.Errorf("partitionConsumer.parseResponse() should be nil, got %v", got)
	}
	if child.offset != 6 {
		t.Errorf("child.offset should be LastRecordsBatchOffset + 1: %d, got %d", lrbOffset+1, child.offset)
	}
}

func testConsumerInterceptor(
	t *testing.T,
	interceptors []ConsumerInterceptor,
	expectationFn func(*testing.T, int, *ConsumerMessage),
) {
	// Given
	broker0 := NewMockBroker(t, 0)

	mockFetchResponse := NewMockFetchResponse(t, 1)
	for i := 0; i < 10; i++ {
		mockFetchResponse.SetMessage("my_topic", 0, int64(i), testMsg)
	}

	broker0.SetHandlerByMap(map[string]MockResponse{
		"MetadataRequest": NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, OffsetOldest, 0).
			SetOffset("my_topic", 0, OffsetNewest, 0),
		"FetchRequest": mockFetchResponse,
	})
	config := NewTestConfig()
	config.Consumer.Interceptors = interceptors
	// When
	master, err := NewConsumer([]string{broker0.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	consumer, err := master.ConsumePartition("my_topic", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		select {
		case msg := <-consumer.Messages():
			expectationFn(t, i, msg)
		case err := <-consumer.Errors():
			t.Error(err)
		}
	}

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()
}

func TestConsumerInterceptors(t *testing.T) {
	tests := []struct {
		name          string
		interceptors  []ConsumerInterceptor
		expectationFn func(*testing.T, int, *ConsumerMessage)
	}{
		{
			name:         "intercept messages",
			interceptors: []ConsumerInterceptor{&appendInterceptor{i: 0}},
			expectationFn: func(t *testing.T, i int, msg *ConsumerMessage) {
				ev, _ := testMsg.Encode()
				expected := string(ev) + strconv.Itoa(i)
				v := string(msg.Value)
				if v != expected {
					t.Errorf("Interceptor should have incremented the value, got %s, expected %s", v, expected)
				}
			},
		},
		{
			name:         "interceptor chain",
			interceptors: []ConsumerInterceptor{&appendInterceptor{i: 0}, &appendInterceptor{i: 1000}},
			expectationFn: func(t *testing.T, i int, msg *ConsumerMessage) {
				ev, _ := testMsg.Encode()
				expected := string(ev) + strconv.Itoa(i) + strconv.Itoa(i+1000)
				v := string(msg.Value)
				if v != expected {
					t.Errorf("Interceptor should have incremented the value, got %s, expected %s", v, expected)
				}
			},
		},
		{
			name:         "interceptor chain with one interceptor failing",
			interceptors: []ConsumerInterceptor{&appendInterceptor{i: -1}, &appendInterceptor{i: 1000}},
			expectationFn: func(t *testing.T, i int, msg *ConsumerMessage) {
				ev, _ := testMsg.Encode()
				expected := string(ev) + strconv.Itoa(i+1000)
				v := string(msg.Value)
				if v != expected {
					t.Errorf("Interceptor should have not changed the value, got %s, expected %s", v, expected)
				}
			},
		},
		{
			name:         "interceptor chain with all interceptors failing",
			interceptors: []ConsumerInterceptor{&appendInterceptor{i: -1}, &appendInterceptor{i: -1}},
			expectationFn: func(t *testing.T, i int, msg *ConsumerMessage) {
				ev, _ := testMsg.Encode()
				expected := string(ev)
				v := string(msg.Value)
				if v != expected {
					t.Errorf("Interceptor should have incremented the value, got %s, expected %s", v, expected)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { testConsumerInterceptor(t, tt.interceptors, tt.expectationFn) })
	}
}
