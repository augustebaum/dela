package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"

	"go.dedis.ch/dela/core/execution/evm"
)

func main() {
	port := flag.String("port", "12347", "the port")

	flag.Parse()

	addr := "127.0.0.1:" + *port
	fmt.Println("listening on", addr)

	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		panic("failed to create addr: " + err.Error())
	}

	evmSvc, err := evm.NewExecution()
	if err != nil {
		log.Fatalf("failed to start evm svc: %v\n", err)
	}

	conn, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		fmt.Println("failed to listen: " + err.Error())
	}

	for {
		tcpCon, err := conn.Accept()
		if err != nil {
			panic("failed to accept: " + err.Error())
		}

		fmt.Println("connected to", tcpCon.RemoteAddr().String())

		res := make([]byte, 8)
		_, err = tcpCon.Read(res)
		if err != nil {
			panic("failed to read: " + err.Error())
		}

		val := binary.LittleEndian.Uint64(res)

		fmt.Printf("received: %d\n", val)

		res, err = evmSvc.ExecuteEVM(res)
		if err != nil {
			log.Fatalf("failed to execute contract: %v", err)
		}

		_, err = tcpCon.Write(res)
		if err != nil {
			panic("failed to write back: " + err.Error())
		}

		err = tcpCon.Close()
		if err != nil {
			panic("failed to close: " + err.Error())
		}
	}
}
