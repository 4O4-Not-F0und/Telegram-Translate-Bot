package translator

import (
	"context"
	"errors"
	"fmt"

	"github.com/4O4-Not-F0und/Gura-Bot/translate/common"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/sirupsen/logrus"
)

const (
	instanceTypeOpenAI = "openai"
)

func init() {
	registerTranslatorInstance(instanceTypeOpenAI, newOpenAIInstance)
}

// TranslatorInstanceOpenAI implements the translation logic using the OpenAI style API.
// It embeds baseTranslator for common functionalities.
type InstanceOpenAI struct {
	name         string
	logger       *logrus.Entry
	aiClient     openai.Client
	systemPrompt string
	model        string
}

// newTranslatorInstanceOpenAI creates and initializes a new TranslatorInstanceOpenAI.
// It validates the provided TranslateConfig and configures the OpenAI client,
// language detector, rate limiter, and other parameters.
// Returns an error if any critical configuration is missing or invalid.
func newOpenAIInstance(conf TranslatorConfig) (c Instance, err error) {
	logger := logrus.WithField("translator_instance", conf.Name)

	openaiOpts := []option.RequestOption{}

	if conf.Token == "" {
		logger.Warn("no API token configured, using empty")
	} else {
		openaiOpts = append(openaiOpts, option.WithAPIKey(conf.Token))
	}
	if conf.Endpoint != "" {
		openaiOpts = append(openaiOpts, option.WithBaseURL(conf.Endpoint))
	}

	if conf.Model == "" {
		err = fmt.Errorf("no openai model configured")
		return
	}

	instance := new(InstanceOpenAI)
	instance.aiClient = openai.NewClient(openaiOpts...)
	instance.model = conf.Model

	// Already validated, just set it
	instance.name = conf.Name
	instance.systemPrompt = conf.SystemPrompt
	instance.logger = logger

	instance.logger.Debugf("initialized OpenAI instance, model: %s, api url: %s",
		instance.model, conf.Endpoint)
	return instance, nil
}

func (t *InstanceOpenAI) Name() string {
	return t.name
}

// Translate sends the given text to the OpenAI API for translation.
// It respects the configured timeout and rate limiter.
// Returns the API's chat completion response or an error.
func (t *InstanceOpenAI) Translate(ctx context.Context, req TranslateRequest) (resp *TranslateResponse, err error) {
	var chatCompletion *openai.ChatCompletion
	chatCompletion, err = t.aiClient.Chat.Completions.New(
		ctx,
		openai.ChatCompletionNewParams{
			Model: t.model,
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage(t.systemPrompt),
				openai.UserMessage(req.Text),
			},
		},
	)

	if err != nil {
		var apiErr = new(openai.Error)
		if errors.As(err, &apiErr) {
			// Mask sensitive data
			req := apiErr.Request.Clone(context.Background())
			req.Header = apiErr.Request.Header.Clone()
			req.Header.Set("Authorization", "********")
			err = fmt.Errorf("%w", &common.HTTPError{
				Err:      err,
				Request:  req,
				Response: apiErr.Response,
			})
		}
		return
	}

	resp = new(TranslateResponse)
	if len(chatCompletion.Choices) > 0 {
		resp.Text = chatCompletion.Choices[0].Message.Content
		resp.TokenUsage.Completion = chatCompletion.Usage.CompletionTokens
		resp.TokenUsage.Prompt = chatCompletion.Usage.PromptTokens
		return
	}
	err = fmt.Errorf("no choice found in response")
	return
}
