package main

import (
	"encoding/base64"
	"errors"
	"slices"
	"sync"

	"github.com/4O4-Not-F0und/Gura-Bot/metrics"
	"github.com/4O4-Not-F0und/Gura-Bot/translate"
	"github.com/4O4-Not-F0und/Gura-Bot/translate/common"
	"github.com/4O4-Not-F0und/Gura-Bot/translate/detector"
	"github.com/4O4-Not-F0und/Gura-Bot/translate/translator"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/sirupsen/logrus"
)

const (
	messageHandleStatePending      = "pending"
	messageHandleStateUnauthorized = "unauthorized"
	messageHandleStateFailed       = "failed"
	messageHandleStateProcessed    = "processed"
	messageHandleStateProcessing   = "processing"
)

var (
	allMessageStates = []string{
		messageHandleStatePending,
		messageHandleStateUnauthorized,
		messageHandleStateProcessing,
		messageHandleStateProcessed,
		messageHandleStateFailed,
	}

	allChatTypes = []string{
		"private",
		"group",
		"supergroup",
		"channel",
	}
)

type BotConfig struct {
	Debug           bool               `yaml:"debug"`
	Token           string             `yaml:"token"`
	MessageSettings BotMessageSettings `yaml:"message_settings"`
	AllowedChats    []int64            `yaml:"allowed_chats"`
	WorkerPoolSize  int                `yaml:"worker_pool_size"`
}

type BotMessageSettings struct {
	DisableNotification bool `yaml:"disable_notification"`
	DisableLinkPreview  bool `yaml:"disable_link_preview"`
}

func newBotConfig() BotConfig {
	return BotConfig{
		MessageSettings: BotMessageSettings{},
		AllowedChats:    make([]int64, 0),
	}
}

type SafeSlice[T comparable] struct {
	*sync.RWMutex
	s []T
}

func newSafeSlice[T comparable](s []T) (ss *SafeSlice[T]) {
	ss = &SafeSlice[T]{
		RWMutex: new(sync.RWMutex),
	}
	ss.New(s)
	return
}

func (ss *SafeSlice[T]) Contains(elem T) bool {
	ss.RLock()
	ok := slices.Contains(ss.s, elem)
	ss.RUnlock()
	return ok
}

func (ss *SafeSlice[T]) New(s []T) {
	ss.Lock()
	ss.s = slices.Clone(s)
	ss.Unlock()
}

func (ss *SafeSlice[T]) Clone() (s []T) {
	ss.RLock()
	s = slices.Clone(ss.s)
	ss.RUnlock()
	return
}

type Bot struct {
	bot              *tgbotapi.BotAPI
	updatesChan      tgbotapi.UpdatesChannel
	translateService *translate.TranslateService
	messageSettings  BotMessageSettings
	allowedChats     *SafeSlice[int64]
	workerPoolSize   int
	configMu         *sync.RWMutex
	stopServeNotify  chan int
}

func newBot(config BotConfig, translateService *translate.TranslateService) (bot *Bot, err error) {
	if config.Token == "" {
		logrus.Fatal("telegram bot token required")
	}

	if config.WorkerPoolSize <= 0 {
		logrus.Fatalf("invalid 'worker_pool_size': %d", config.WorkerPoolSize)
	}
	logrus.Info("authorizing telegram bot")

	var botApi *tgbotapi.BotAPI
	botApi, err = tgbotapi.NewBotAPI(config.Token)
	if err != nil {
		return
	}
	logrus.Infof("authorized on account: %s", botApi.Self.UserName)
	botApi.Debug = config.Debug

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := botApi.GetUpdatesChan(u)

	bot = &Bot{
		bot:              botApi,
		updatesChan:      updates,
		translateService: translateService,
		messageSettings:  config.MessageSettings,
		allowedChats:     newSafeSlice(config.AllowedChats),
		workerPoolSize:   config.WorkerPoolSize,
		configMu:         &sync.RWMutex{},
		stopServeNotify:  make(chan int, 1),
	}

	_, err = bot.loadConfig(config, translateService)
	if err != nil {
		return
	}

	bot.initMessageMetrics()
	return
}

func (b *Bot) loadConfig(botConfig BotConfig, translateService *translate.TranslateService) (reServeRequired bool, err error) {
	logrus.Trace("acquiring bot.configMu")
	b.configMu.Lock()
	defer b.configMu.Unlock()
	logrus.Trace("acquired bot.configMu")

	b.allowedChats.New(botConfig.AllowedChats)
	b.messageSettings = botConfig.MessageSettings
	b.translateService = translateService
	reServeRequired = b.workerPoolSize != botConfig.WorkerPoolSize
	b.workerPoolSize = botConfig.WorkerPoolSize

	logrus.Trace("released bot.configMu")
	return
}

func (b *Bot) Reload(botConfig BotConfig, translateService *translate.TranslateService) (err error) {
	var reServeRequired bool
	reServeRequired, err = b.loadConfig(botConfig, translateService)
	if err != nil {
		return
	}

	if reServeRequired {
		logrus.Info("re-serve bot required, attempting to restart bot loop")
		b.stopServeNotify <- 1
		go b.ServeBot()
	}

	return
}

// ServeBot starts the bot's main loop for receiving and processing updates.
func (b *Bot) ServeBot() {
	q := make(chan int, b.workerPoolSize)

	logrus.Infof("begin update loop, queue size: %d", b.workerPoolSize)
	defer func() {
		logrus.Info("stopped update loop")
	}()
	for update := range b.updatesChan {
		select {
		case <-b.stopServeNotify:
			return
		default:
		}

		var msg *Message
		if update.Message != nil {
			msg = newMessage(update.Message)
		} else if update.ChannelPost != nil {
			msg = newMessage(update.ChannelPost)
		} else {
			continue
		}

		if msg.Content == "" {
			msg.logger.Debug("message text undetected")
			continue
		}

		msg.onPending()
		logrus.Trace("acquiring queue")
		q <- 1
		msg.onProcessing()
		logrus.Trace("acquired queue")

		go func(m *Message) {
			b.handleMessage(m)
			<-q
			logrus.Trace("released queue")
		}(msg)
	}
}

// handleMessage processes a single incoming Telegram message.
// It checks for authorization, extracts text, detects language,
// translates, and sends a reply.
func (b *Bot) handleMessage(msg *Message) {
	defer func() {
		if r := recover(); r != nil {
			msg.logger.Errorf("panic recovered in handleMessage: %v", r)
			msg.onMessageHandleFailed()
		}
	}()

	if !b.isAllowed(msg) {
		msg.onUnauthorized()
		return
	}

	langResp, detectorName, err := b.translateService.DetectLang(detector.DetectRequest{
		Text:    msg.Content,
		TraceId: msg.TraceId,
	})
	if detectorName != "" {
		msg.logger = msg.logger.WithField("detector_name", detectorName)
	}
	if langResp != nil {
		msg.logger = msg.logger.WithFields(logrus.Fields{
			"lang":            langResp.Language,
			"lang_confidence": langResp.Confidence,
		})
	}
	if err != nil {
		msg.logger.Warn(err)
		msg.onMessageHandleFailed()
		return
	}

	resp, translatorName, err := b.translateService.Translate(translator.TranslateRequest{
		Text:    msg.Content,
		TraceId: msg.TraceId,
	})
	if translatorName != "" {
		msg.logger = msg.logger.WithField("translator_name", translatorName)
	}
	if err != nil {
		msg.onMessageHandleFailed()

		var te = new(common.HTTPError)
		if errors.As(err, &te) {
			msg.logger.Debugf("http request: %s", base64.StdEncoding.EncodeToString(te.DumpRequest(true)))
			msg.logger.Debugf("http response: %s", base64.StdEncoding.EncodeToString(te.DumpResponse(true)))
		}
		msg.logger.Errorf("an error occurred while translating: %v", err)
		return
	}

	msg.logger = msg.logger.WithFields(logrus.Fields{
		"usage_completion_tokens": resp.TokenUsage.Completion,
		"usage_prompt_tokens":     resp.TokenUsage.Prompt,
	})

	reply := tgbotapi.NewMessage(msg.Chat.ID, resp.Text)
	b.configMu.RLock()
	reply.DisableNotification = b.messageSettings.DisableNotification
	reply.DisableWebPagePreview = b.messageSettings.DisableLinkPreview
	b.configMu.RUnlock()
	reply.ReplyToMessageID = msg.MessageID

	_, err = b.bot.Send(reply)
	if err != nil {
		msg.onMessageHandleFailed()
		msg.logger.Errorf("an error occurred while replying message: %v", err)
	}
	msg.logger.Info("completed")
	msg.onSuccess()
}

func (b *Bot) initMessageMetrics() {
	for _, ct := range allChatTypes {
		for _, state := range allMessageStates {
			metrics.MetricMessages.WithLabelValues(state, ct).Set(0)
		}
	}

	logrus.Info("all bot metrics initialized")
}

func (b *Bot) isAllowed(message *Message) bool {
	if message.Chat.Type == "private" {
		return b.allowedChats.Contains(message.From.ID)
	}
	return b.allowedChats.Contains(message.Chat.ID)
}
