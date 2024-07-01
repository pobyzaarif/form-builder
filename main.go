package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"text/template"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-playground/validator/v10"
	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

type Field struct {
	Label       string `json:"label" validate:"required"`
	Type        string `json:"type" validate:"required,fieldtype"`
	Name        string `json:"name" validate:"required,alphanum"`
	Placeholder string `json:"placeholder"`
}

type Form struct {
	Title  string  `json:"title" validate:"required"`
	Fields []Field `json:"fields" validate:"required,dive"`
}

var (
	versionAPP      = "1.0.0"
	validFieldTypes = map[string]bool{
		"text":     true,
		"email":    true,
		"textarea": true,
		"number":   true,
		"date":     true,
		"checkbox": true,
	}

	allowedClientKeys = make([]string, 0)
)

func fieldTypeValidator(fl validator.FieldLevel) bool {
	fieldType := fl.Field().String()
	_, valid := validFieldTypes[fieldType]
	return valid
}

func main() {
	spew.Dump() // i usually use this to debug

	conf, err := NewConfig()
	if err != nil {
		log.Panic(err)
	}

	// c := cache.New(conf.Cache.DEFAULT_EXPIRATION, conf.Cache.CLEANUP_INTERVAL)

	json.Unmarshal([]byte(conf.ClientKeys), &allowedClientKeys)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	registerPath(e, &conf.APPConfig)

	// Validator
	validate := validator.New()
	validate.RegisterValidation("fieldtype", fieldTypeValidator)
	e.Validator = &CustomValidator{validator: validate}

	e.GET("/", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"message": "form builder", "version": versionAPP})
	})

	// Start server
	address := conf.APPConfig.Host + ":" + conf.APPConfig.Port
	log.Printf("service running in %v", address)
	go func() {
		if err := e.Start(address); err != http.ErrServerClosed {
			log.Fatal("failed on http server " + conf.APPConfig.Port)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
	<-quit

	// a timeout of 10 seconds to shutdown the server
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := e.Shutdown(ctx); err != nil {
		log.Fatalf("failed to shutting down echo server %v", err)
	} else {
		log.Print("successfully shutting down echo server")
	}

}

type Config struct {
	APPConfig  APPConfig   `envPrefix:"APP_"`
	Cache      CacheConfig `envPrefix:"IN_MEMORY_CACHE_"`
	ClientKeys string      `env:"CLIENT_KEYS" envDefault:"[\"api-key-1\",\"api-key-2\"]"`
}

type CacheConfig struct {
	DEFAULT_EXPIRATION time.Duration `env:"DEFAULT_EXPIRATION" envDefault:"720m"`
	CLEANUP_INTERVAL   time.Duration `env:"CLEANUP_INTERVAL" envDefault:"10m"`
}

type APPConfig struct {
	Host   string `env:"HOST" envDefault:"0.0.0.0"`
	Port   string `env:"PORT" envDefault:"8080"`
	Domain string `env:"DOMAIN" envDefault:"http://0.0.0.0:8080"`
}

// NewConfig creates a new Config.
func NewConfig() (*Config, error) {
	_ = godotenv.Load(".env")

	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		panic(err)
	}

	return &cfg, nil
}

type CustomValidator struct {
	validator *validator.Validate
}

func (cv *CustomValidator) Validate(i interface{}) error {
	return cv.validator.Struct(i)
}

// Custom middleware to check x-client-key header
func clientKeyMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		clientKey := c.Request().Header.Get("x-client-key")
		if clientKey == "" {
			return c.JSON(http.StatusForbidden, map[string]string{"message": "x-client-key header is required"})
		}
		for _, v := range allowedClientKeys {
			if clientKey == v {
				c.Set("clientKey", clientKey)
				return next(c)
			}
		}
		return c.JSON(http.StatusForbidden, map[string]string{"message": "key is not found"})
	}
}

// makeHTML parse template and make html
func makeHTML(templateFileName string, data interface{}) (string, error) {
	t, err := template.ParseFiles(templateFileName)
	if err != nil {
		return "", err
	}
	buf := new(bytes.Buffer)
	if err = t.Execute(buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Check if file exists
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

// Load JSON from file
func loadJSON(path string) (map[string]interface{}, error) {
	var form map[string]interface{}

	file, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(file, &form)
	if err != nil {
		return nil, err
	}

	return form, nil
}

// sortByKey sorts the input map by its keys and returns a new map with the sorted order.
func sortByKey(in map[string]string) map[string]string {
	// Extract the keys from the map
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}

	// Sort the keys
	sort.Strings(keys)

	// Create a new map and insert the elements in the sorted order
	out := make(map[string]string)
	for _, key := range keys {
		out[key] = in[key]
	}

	return out
}

// Register API path
func registerPath(e *echo.Echo, appConf *APPConfig) {
	failedMissingMandatoryParameterMsg := map[string]string{"message": "Missing mandatory parameter"}
	failedNotfoundMsg := map[string]string{"message": "Failed get form because the form not found or no longer exists"}

	api := e.Group("/api")
	// api.Use(clientKeyMiddleware)

	// Route to save the form JSON
	api.POST("/save-form", func(c echo.Context) error {
		form := new(Form)
		if err := c.Bind(form); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"message": "Invalid form structure"})
		}

		if err := c.Validate(form); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"message": err.Error()})
		}

		// Create the directory with the timestamp
		timestamp := time.Now().UnixMilli()
		dir := filepath.Join("form-build", fmt.Sprintf("%d", timestamp))
		if err := os.MkdirAll(dir, os.ModePerm); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Failed to create directory"})
		}

		// Create the form.json file
		filePath := filepath.Join(dir, "form.json")
		file, err := os.Create(filePath)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Failed to create file"})
		}
		defer file.Close()

		// Write the JSON to the file
		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(form); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Failed to write to file"})
		}

		return c.JSON(http.StatusOK, map[string]string{"message": "Form saved successfully", "path": filePath})
	}, clientKeyMiddleware)

	api.GET("/get-form/:formID", func(c echo.Context) error {
		ID := c.Param("formID")
		if ID == "" {
			return c.JSON(http.StatusBadRequest, failedMissingMandatoryParameterMsg)
		}

		filePath := filepath.Join("form-build", ID, "form.json")
		if !fileExists(filePath) {
			return c.JSON(http.StatusNotFound, failedNotfoundMsg)
		}

		jsonData, err := loadJSON(filePath)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
		}

		jsonStr, _ := json.Marshal(jsonData)
		form, err := makeHTML(filepath.Join("form-build", "form.html"), map[string]interface{}{
			"data":         string(jsonStr),
			"url":          appConf.Domain + filepath.Join("/api/submit-form/", ID),
			"clientXToken": "x.y.z",
		})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
		}

		return c.HTML(http.StatusOK, form)
	})

	api.POST("/submit-form/:formID", func(c echo.Context) error {
		formData := make(map[string]string)
		if err := c.Bind(&formData); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"message": "Invalid form data"})
		}

		if formData["formID"] == "" {
			return c.JSON(http.StatusBadRequest, map[string]string{"message": "Invalid form data"})
		}

		// TODO check token

		// Create the directory if it doesn't exist
		dir := filepath.Join("form-build", formData["formID"])
		if err := os.MkdirAll(dir, os.ModePerm); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Failed to create directory"})
		}

		// remove unused fields
		referrer := formData["referrer"]
		delete(formData, "formID")
		delete(formData, "clientXToken")
		delete(formData, "referrer")

		formData = sortByKey(formData)

		// Append the form data to the CSV file
		csvFilePath := filepath.Join(dir, "form_answer.csv")
		file, err := os.OpenFile(csvFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Failed to open CSV file"})
		}
		defer file.Close()

		// Check if file is new by checking its size
		info, err := file.Stat()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Failed to get file info"})
		}
		isNewFile := info.Size() == 0

		var s sync.Mutex
		s.Lock()
		defer s.Unlock()

		writer := csv.NewWriter(file)
		defer writer.Flush()

		// Write headers if new file
		if isNewFile {
			headers := make([]string, 0, len(formData))
			for key := range formData {
				headers = append(headers, key)
			}
			if err := writer.Write(headers); err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Failed to write headers to CSV file"})
			}
		}

		record := []string{}
		for _, value := range formData {
			record = append(record, value)
		}
		if err := writer.Write(record); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Failed to write to CSV file"})
		}

		htmlResponse := `<meta http-equiv="refresh" content="3;url=` + referrer + `" />
		<h1>Form submitted successfully, you will redirect previous page in 3 seconds</h1>
		`

		return c.HTML(http.StatusOK, htmlResponse)
	})
}
