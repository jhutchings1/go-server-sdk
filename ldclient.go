package ldclient

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/gregjones/httpcache"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const (
	long_scale = float32(0xFFFFFFFFFFFFFFF)
)

var Version string = "0.0.3"

type User struct {
	Key       *string                 `json:"key,omitempty" bson:"key,omitempty"`
	Secondary *string                 `json:"secondary,omitempty" bson:"secondary,omitempty"`
	Ip        *string                 `json:"ip,omitempty" bson:"ip,omitempty"`
	Country   *string                 `json:"country,omitempty" bson:"country,omitempty"`
	Email     *string                 `json:"email,omitempty" bson:"email,omitempty"`
	FirstName *string                 `json:"firstName,omitempty" bson:"firstName,omitempty"`
	LastName  *string                 `json:"lastName,omitempty" bson:"lastName,omitempty"`
	Avatar    *string                 `json:"avatar,omitempty" bson:"avatar,omitempty"`
	Name      *string                 `json:"name,omitempty" bson:"name,omitempty"`
	Custom    *map[string]interface{} `json:"custom,omitempty" bson:"custom,omitempty"`
}

type Operator string

type Feature struct {
	Name         *string      `json:"name"`
	Key          *string      `json:"key"`
	Kind         *string      `json:"kind"`
	Salt         *string      `json:"salt"`
	On           *bool        `json:"on"`
	Variations   *[]Variation `json:"variations"`
	Ttl          *int         `json:"ttl"`
	CommitDate   *time.Time   `json:"commitDate"`
	CreationDate *time.Time   `json:"creationDate"`
}

type TargetRule struct {
	Attribute string        `json:"attribute"`
	Op        Operator      `json:"op"`
	Values    []interface{} `json:"values"`
}

type Variation struct {
	Value      interface{}  `json:"value"`
	Weight     int          `json:"weight"`
	Targets    []TargetRule `json:"targets"`
	UserTarget *TargetRule  `json:"userRule,omitempty"`
}

type LDClient struct {
	apiKey     string
	config     Config
	httpClient *http.Client
	processor  *eventProcessor
	offline    bool
}

type Config struct {
	BaseUri       string
	Capacity      int
	FlushInterval time.Duration
	Logger        *log.Logger
	Timeout       time.Duration
}

var DefaultConfig = Config{
	BaseUri:       "https://app.launchdarkly.com",
	Capacity:      1000,
	FlushInterval: 5 * time.Second,
	Logger:        log.New(os.Stderr, "[LaunchDarkly]", log.LstdFlags),
	Timeout:       1500 * time.Millisecond,
}

func MakeCustomClient(apiKey string, config Config) LDClient {
	config.BaseUri = strings.TrimRight(config.BaseUri, "/")
	httpClient := httpcache.NewMemoryCacheTransport().Client()
	httpClient.Timeout = config.Timeout

	return LDClient{
		apiKey:     apiKey,
		config:     config,
		httpClient: httpClient,
		processor:  newEventProcessor(apiKey, config),
		offline:    false,
	}
}

func MakeClient(apiKey string) *LDClient {
	res := MakeCustomClient(apiKey, DefaultConfig)
	return &res
}

func (b Feature) paramForId(user User) (float32, bool) {
	var idHash string

	if user.Key != nil {
		idHash = *user.Key
	} else { // without a key, this rule should pass
		return 0, true
	}

	if user.Secondary != nil {
		idHash = idHash + "." + *user.Secondary
	}

	h := sha1.New()
	io.WriteString(h, *b.Key+"."+*b.Salt+"."+idHash)
	hash := hex.EncodeToString(h.Sum(nil))[:15]

	intVal, _ := strconv.ParseInt(hash, 16, 64)

	param := float32(intVal) / long_scale

	return param, false
}

func matchCustom(target TargetRule, user User) bool {
	if user.Custom == nil {
		return false
	}
	var v interface{} = (*user.Custom)[target.Attribute]

	if v == nil {
		return false
	}

	val := reflect.ValueOf(v)

	if val.Kind() == reflect.Array || val.Kind() == reflect.Slice {
		for i := 0; i < val.Len(); i++ {
			if compareValues(val.Index(i).Interface(), target.Values) {
				return true
			}
		}
		return false
	} else {
		return compareValues(v, target.Values)
	}
}

func compareValues(value interface{}, values []interface{}) bool {
	if value == "" {
		return false
	} else {
		for _, v := range values {
			if value == v {
				return true
			}
		}
	}
	return false
}

func (target TargetRule) matchTarget(user User) bool {
	var uValue string
	if target.Attribute == "key" {
		if user.Key != nil {
			uValue = *user.Key
		}
	} else if target.Attribute == "ip" {
		if user.Ip != nil {
			uValue = *user.Ip
		}
	} else if target.Attribute == "country" {
		if user.Country != nil {
			uValue = *user.Country
		}
	} else if target.Attribute == "email" {
		if user.Email != nil {
			uValue = *user.Email
		}
	} else if target.Attribute == "firstName" {
		if user.FirstName != nil {
			uValue = *user.FirstName
		}
	} else if target.Attribute == "lastName" {
		if user.LastName != nil {
			uValue = *user.LastName
		}
	} else if target.Attribute == "avatar" {
		if user.Avatar != nil {
			uValue = *user.Avatar
		}
	} else if target.Attribute == "name" {
		if user.Name != nil {
			uValue = *user.Name
		}
	} else {
		if matchCustom(target, user) {
			return true
		} else {
			return false
		}
	}

	if compareValues(uValue, target.Values) {
		return true
	} else {
		return false
	}
}

func (variation Variation) matchTarget(user User) *TargetRule {
	for _, target := range variation.Targets {
		if variation.UserTarget != nil && target.Attribute == "key" {
			continue
		}
		if target.matchTarget(user) {
			return &target
		}
	}
	return nil
}

func (variation Variation) matchUser(user User) *TargetRule {
	if variation.UserTarget != nil && variation.UserTarget.matchTarget(user) {
		return variation.UserTarget
	}
	return nil
}

func (f Feature) Evaluate(user User) (value interface{}, rulesPassed bool) {
	value, _, rulesPassed = f.EvaluateExplain(user)
	return
}

func (f Feature) EvaluateExplain(user User) (value interface{}, targetMatch *TargetRule, rulesPassed bool) {

	if !*f.On {
		return nil, nil, true
	}

	param, passErr := f.paramForId(user)

	if passErr {
		return nil, nil, true
	}

	for _, variation := range *f.Variations {
		target := variation.matchUser(user)
		if target != nil {
			return variation.Value, target, false
		}
	}

	for _, variation := range *f.Variations {
		target := variation.matchTarget(user)
		if target != nil {
			return variation.Value, target, false
		}

	}

	var sum float32 = 0.0

	for _, variation := range *f.Variations {
		sum += float32(variation.Weight) / 100.0
		if param < sum {
			return variation.Value, nil, false
		}
	}

	return nil, nil, true
}

func (client *LDClient) Identify(user User) error {
	if client.offline {
		return nil
	}
	evt := newIdentifyEvent(user)
	return client.processor.sendEvent(evt)
}

func (client *LDClient) Track(key string, user User, data interface{}) error {
	if client.offline {
		return nil
	}
	evt := newCustomEvent(key, user, data)
	return client.processor.sendEvent(evt)
}

func (client *LDClient) sendFlagRequestEvent(key string, user User, value interface{}) error {
	if client.offline {
		return nil
	}
	evt := newFeatureRequestEvent(key, user, value)
	return client.processor.sendEvent(evt)
}

func (client *LDClient) SetOffline() {
	client.offline = true
}

func (client *LDClient) SetOnline() {
	client.offline = false
}

func (client *LDClient) IsOffline() bool {
	return client.offline
}

func (client *LDClient) Close() {
	client.processor.close()
}

func (client *LDClient) GetFlag(key string, user User, defaultVal bool) (bool, error) {
	if client.IsOffline() {
		return defaultVal, nil
	}

	req, reqErr := http.NewRequest("GET", client.config.BaseUri+"/api/eval/features/"+key, nil)

	if reqErr != nil {
		client.sendFlagRequestEvent(key, user, defaultVal)
		return defaultVal, reqErr
	}

	req.Header.Add("Authorization", "api_key "+client.apiKey)
	req.Header.Add("User-Agent", "GoClient/"+Version)

	res, resErr := client.httpClient.Do(req)

	defer func() {
		if res != nil && res.Body != nil {
			ioutil.ReadAll(res.Body)
			res.Body.Close()
		}
	}()

	if resErr != nil {
		client.sendFlagRequestEvent(key, user, defaultVal)
		return defaultVal, resErr
	}

	if res.StatusCode == http.StatusUnauthorized {
		client.sendFlagRequestEvent(key, user, defaultVal)
		return defaultVal, errors.New("Invalid API key. Verify that your API key is correct. Returning default value.")
	}

	if res.StatusCode == http.StatusNotFound {
		client.sendFlagRequestEvent(key, user, defaultVal)
		return defaultVal, errors.New("Invalid feature key. Verify that this feature key exists. Returning default value.")
	}

	if res.StatusCode != http.StatusOK {
		client.sendFlagRequestEvent(key, user, defaultVal)
		return defaultVal, errors.New("Unexpected response code: " + strconv.Itoa(res.StatusCode))
	}

	body, err := ioutil.ReadAll(res.Body)

	if err != nil {
		client.sendFlagRequestEvent(key, user, defaultVal)
		return defaultVal, err
	}

	var feature Feature
	jsonErr := json.Unmarshal(body, &feature)

	if jsonErr != nil {
		client.sendFlagRequestEvent(key, user, defaultVal)
		return defaultVal, jsonErr
	}

	value, pass := feature.Evaluate(user)

	if pass {
		client.sendFlagRequestEvent(key, user, defaultVal)
		return defaultVal, nil
	}

	result, ok := value.(bool)

	if !ok {
		client.sendFlagRequestEvent(key, user, defaultVal)
		return defaultVal, errors.New("Feature flag returned non-bool value")
	}

	client.sendFlagRequestEvent(key, user, result)
	return result, nil
}
