package bad

import (
	"log"
	"os"
)

func Ignored() {
	_ = os.Remove("x")
	if err := os.Remove("y"); err != nil {
		log.Printf("remove: %v", err)
	}
}

func Handled() error {
	if err := os.Remove("z"); err != nil {
		log.Printf("remove: %v", err)
		return err
	}
	var x int
	_ = x
	return nil
}
