package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gandalfthegui/grove/internal/proto"
	"golang.org/x/term"
)

var watchTreeLeft = []string{
	`     ccee88oo`,
	`  C8O8O8Q8PoOb o8oo`,
	` dOB69QO8PdUOpugoO9bD`,
	`CgggbU8OU qOp qOdoUOdcb`,
	`    6OuU  /p u gcoUodpP`,
	`      \\\//  /douUP`,
	`        \\\////`,
	`         |||/\`,
	`         |||\/`,
	`         |||||`,
	`   .....//||||\....`,
}

var watchTreeRight = []string{
	`        ccee88oo`,
	`  C8O8O8Q8PoOb o8oo`,
	` dOB9_GandalftheGUI_O9bD`,
	`CgggbU8OU qOp qOdoUOdcb`,
	`    6OuU6 /p IRgcoUodpP`,
	`      \dou/  /douUP`,
	`        \\\\///`,
	`         |||||`,
	`         |ILR|`,
	`         |||||`,
	`   .....//||||\....`,
}

var watchBanner = []string{
	"      _,---.                 _,.---._           ,-.-.    ,----. ",
	"  _.='.'-,  \\  .-.,.---.   ,-.' , -  `.  ,--.-./=/ ,/ ,-.--` , \\",
	" /==.'-     / /==/  `   \\ /==/_,  ,  - \\/==/, ||=| -||==|-  _.-`",
	"/==/ -   .-' |==|-, .=., |==|   .=.     \\==\\,  \\ / ,||==|   `.-.",
	"|==|_   /_,-.|==|   '='  /==|_ : ;=:  - |\\==\\ - ' - /==/_ ,    /",
	"|==|  , \\_.' )==|- ,   .'|==| , '='     | \\==\\ ,   ||==|    .-' ",
	"\\==\\-  ,    (|==|_  . ,'. \\==\\ -    ,_ /  |==| -  ,/|==|_  ,`-._",
	" /==/ _  ,  //==/  /\\ ,  ) '.='. -   .'   \\==\\  _ / /==/ ,     /",
	" `--`------' `--`-`--`--'    `--`--''      `--`--'  `--`-----`` ",
}

func cmdWatch() {
	socketPath := daemonSocket()

	fd := int(os.Stdout.Fd())

	// Enter alternate screen buffer; restore on exit.
	fmt.Print("\033[?1049h\033[?25l")
	defer fmt.Print("\033[?25h\033[?1049l")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)
	defer signal.Stop(winchCh)

	drawWatch(fd, socketPath)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			fmt.Print("\033[?25h\033[?1049l")
			os.Exit(0)
		case <-winchCh:
			drawWatch(fd, socketPath)
		case <-ticker.C:
			drawWatch(fd, socketPath)
		}
	}
}

func drawWatch(fd int, socketPath string) {
	width, _, err := term.GetSize(fd)
	if err != nil || width < 40 {
		width = 120
	}

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Printf("\033[Hdaemon not reachable: %v\n\033[J", err)
		return
	}
	defer conn.Close()

	if err := writeRequest(conn, proto.Request{Type: proto.ReqList}); err != nil {
		fmt.Printf("\033[Hdaemon not reachable: %v\n\033[J", err)
		return
	}
	resp, err := readResponse(conn)
	if err != nil || !resp.OK {
		fmt.Printf("\033[Hdaemon not reachable: %v\n\033[J", err)
		return
	}

	// Compute dynamic column widths based on actual content.
	const idW, stateW, uptimeW = 10, 10, 10
	projW := 14 // minimum width
	for _, inst := range resp.Instances {
		if l := len(inst.Project); l > projW {
			projW = l
		}
	}
	if projW > 30 {
		projW = 30
	}

	const separators = 4 * 2 // 4 column gaps of 2 spaces
	branchW := width - (idW + projW + stateW + uptimeW + separators)
	if branchW < 15 {
		branchW = 15
	}

	var buf strings.Builder
	buf.WriteString("\033[H")

	// ASCII art grove header — banner with tree on either side.
	const treeGap = 2
	maxTreeW := 0
	for _, l := range watchTreeLeft {
		if len(l) > maxTreeW {
			maxTreeW = len(l)
		}
	}
	for _, l := range watchTreeRight {
		if len(l) > maxTreeW {
			maxTreeW = len(l)
		}
	}
	maxBannerW := 0
	for _, l := range watchBanner {
		if len(l) > maxBannerW {
			maxBannerW = len(l)
		}
	}
	// 11 rows: tree (11 lines) + banner (9 lines, padded with blank at top and bottom).
	bannerPadded := make([]string, 11)
	bannerPadded[0] = ""
	bannerPadded[10] = ""
	for i := 0; i < 9; i++ {
		bannerPadded[1+i] = watchBanner[i]
	}
	rowWidth := maxTreeW + treeGap + maxBannerW + treeGap + maxTreeW
	leftRowPad := (width - rowWidth) / 2
	if leftRowPad < 0 {
		leftRowPad = 0
	}
	buf.WriteString("\033[32m") // green for the trees
	for i := 0; i < 11; i++ {
		leftLine := watchTreeLeft[i]
		if len(leftLine) < maxTreeW {
			leftLine = leftLine + strings.Repeat(" ", maxTreeW-len(leftLine))
		}
		rightLine := watchTreeRight[i]
		if len(rightLine) < maxTreeW {
			rightLine = rightLine + strings.Repeat(" ", maxTreeW-len(rightLine))
		}
		bannerLine := bannerPadded[i]
		if len(bannerLine) < maxBannerW {
			bannerLine = bannerLine + strings.Repeat(" ", maxBannerW-len(bannerLine))
		}
		row := leftLine + strings.Repeat(" ", treeGap) + bannerLine + strings.Repeat(" ", treeGap) + rightLine
		if leftRowPad > 0 {
			buf.WriteString(strings.Repeat(" ", leftRowPad))
		}
		buf.WriteString(row + "\n")
	}
	buf.WriteString("\033[0m\n")

	// Column headers.
	fmt.Fprintf(&buf, "%-*s  %-*s  %-*s  %-*s  %s\n",
		idW, "ID", projW, "PROJECT", stateW, "STATE", uptimeW, "UPTIME", "BRANCH")
	fmt.Fprintf(&buf, "\033[2m%s  %s  %s  %s  %s\033[0m\n",
		strings.Repeat("─", idW),
		strings.Repeat("─", projW),
		strings.Repeat("─", stateW),
		strings.Repeat("─", uptimeW),
		strings.Repeat("─", branchW))

	now := time.Now().Unix()
	var running int
	for _, inst := range resp.Instances {
		project := truncate(inst.Project, projW)
		branch := truncate(inst.Branch, branchW)
		uptimeEnd := now
		if inst.EndedAt > 0 {
			uptimeEnd = inst.EndedAt
		}
		uptime := formatUptime(uptimeEnd - inst.CreatedAt)
		stateColored := colorState(inst.State)
		fmt.Fprintf(&buf, "%-*s  %-*s  %s%-*s\033[0m  %-*s  %s\n",
			idW, inst.ID,
			projW, project,
			stateColored, stateW, inst.State,
			uptimeW, uptime,
			branch)
		if inst.State == "RUNNING" || inst.State == "ATTACHED" {
			running++
		}
	}

	if len(resp.Instances) == 0 {
		buf.WriteString("\n  no instances running\n")
	}

	// Status footer.
	fmt.Fprintf(&buf, "\n\033[2m  %d instance(s)  ·  %d running  ·  %s\033[0m\n",
		len(resp.Instances), running, time.Now().Format("15:04:05"))

	buf.WriteString("\033[J")
	fmt.Print(buf.String())
}
