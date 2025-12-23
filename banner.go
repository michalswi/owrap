package main

import (
	"fmt"

	"github.com/michalswi/color"
)

var banner = `
 ██████╗ ██╗    ██╗██████╗  █████╗ ██████╗ 
██╔═══██╗██║    ██║██╔══██╗██╔══██╗██╔══██╗
██║   ██║██║ █╗ ██║██████╔╝███████║██████╔╝
██║   ██║██║███╗██║██╔══██╗██╔══██║██╔═══╝ 
╚██████╔╝╚███╔███╔╝██║  ██║██║  ██║██║     
 ╚═════╝  ╚══╝╚══╝ ╚═╝  ╚═╝╚═╝  ╚═╝╚═╝
	` + version + ` - @michalswi

`

func ShowBanner() {
	fmt.Printf("%s", color.Format(color.BLUE, banner))
}
