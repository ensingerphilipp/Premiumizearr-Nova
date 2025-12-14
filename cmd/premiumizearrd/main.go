package main

import (
	"flag"
	"fmt"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/utils"
)

const asciiArt = ".______   .______      _______ .___  ___.  __   __    __  .___  ___.  __   ________   _______     ___      .______     .______                   \n" +
	"|   _  \\  |   _  \\    |   ____||   \\/   | |  | |  |  |  | |   \\/   | |  | |       /  |   ____|   /   \\     |   _  \\    |   _  \\                  \n" +
	"|  |_)  | |  |_)  |   |  |__   |  \\  /  | |  | |  |  |  | |  \\  /  | |  |  ---/  /   |  |__     /  ^  \\    |  |_)  |   |  |_)  |                 \n" +
	"|   ___/  |      /    |   __|  |  |\\/|  | |  | |  |  |  | |  |\\/|  | |  |     /  /    |   __|   /  /_\\  \\   |      /    |      /                  \n" +
	"|  |      |  |\\  \\----|  |____ |  |  |  | |  | |  '--'  | |  |  |  | |  |    /  /----.|  |____ /  _____  \\  |  |\\  \\----|  |\\  \\----.             \n" +
	"| _|      | _|  `._____|_______||__|  |__| |__|  \\______/  |__|  |__| |__|   /________||_______/__/     \\__\\ | _|  `._____| _|  `.__|             \n" +
	"                                                                                                                                                  \n" +
	"                                                                                                     .__   __.   ______   ____    ____  ___      \n" +
	"                                                                                                     |  \\ |  |  /  __  \\  \\   \\  /   / /   \\     \n" +
	"                                                                                           ______    |   \\|  | |  |  |  |  \\   \\/   / /  ^  \\    \n" +
	"                                                                                          |______|   |  . `  | |  |  |  |   \\      / /  /_\\  \\   \n" +
	"                                                                                                     |  |\\   | |  '--'  |    \\    / /  _____  \\  \n" +
	"                                                                                                     |__| \\__|  \\______/      \\__/ /__/     \\__\\ \n" +
	"                                                                                                      Version: 1.4.9                      \n"

func main() {
	//Flags
	var logLevel string
	var configFile string
	var loggingDirectory string

	//Parse flags
	fmt.Println(asciiArt)
	fmt.Println("Premiumizearr-Nova Version: 1.4.9")
	flag.StringVar(&logLevel, "log", utils.EnvOrDefault("PREMIUMIZEARR_LOG_LEVEL", "info"), "Logging level: \n \tinfo,debug,trace")
	flag.StringVar(&configFile, "config", utils.EnvOrDefault("PREMIUMIZEARR_CONFIG_DIR_PATH", "./"), "The directory the config.yml is located in")
	flag.StringVar(&loggingDirectory, "logging-dir", utils.EnvOrDefault("PREMIUMIZEARR_LOGGING_DIR_PATH", "./"), "The directory logs are to be written to")
	flag.Parse()

	App := &App{}
	App.Start(logLevel, configFile, loggingDirectory)

}
