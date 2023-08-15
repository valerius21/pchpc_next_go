package main

import (
	"flag"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	mpi "github.com/sbromberger/gompi"
	"os"
	"pchpc_next/streets"
	"strconv"
	"sync"
)

func main() {
	// Flags
	n := flag.Int("n", 100, "Number of vehicles")
	useRoutines := flag.Bool("m", false, "Use goroutines")
	minSpeed := flag.Float64("min-speed", 5.5, "Minimum speed")
	maxSpeed := flag.Float64("max-speed", 8.5, "Maximum speed")
	jsonPath := flag.String("jsonPath", "assets/out.json", "Path to the json containing the graph data")
	debug := flag.Bool("debug", false, "Enable debug mode")
	useMPI := flag.Bool("mpi", false, "Use MPI")

	flag.Parse()

	setupLogging(debug)

	b := streets.NewGraphBuilder().FromJsonFile(*jsonPath).SetTopRightBottomLeftVertices()
	rootGraph, err := b.NumberOfRects(1).DivideGraphsIntoRects().PickRect(0).IsRoot().Build()
	if err != nil {
		log.Error().Err(err).Msg("Failed to build graph")
		return
	}

	// Create vehicles and drive
	ns := strconv.Itoa(*n)
	log.Info().Msg("Starting vehicles " + ns)

	vehicleList := make([]*streets.Vehicle, *n)

	if connectVehiclesToGraph(n, rootGraph, minSpeed, maxSpeed, vehicleList) {
		log.Error().Err(err).Msg("Failed to add vehicle")
		return
	}

	if !*useMPI {
		log.Info().Msg("Running without MPI")
		if *useRoutines {
			runWithGoRoutines(vehicleList)
		} else {
			runSequentially(vehicleList)
		}
		return
	}

	mpi.Start(false)
	defer mpi.Stop()
	comm := mpi.NewCommunicator(nil)

	//numTasks := world.Size()
	taskID := comm.Rank()

	if mpi.WorldSize() < 2 {
		log.Error().Msg("World size is less than 2")
		return
	}

	// I.3 every process will divide the graph into rectangles
	rectangularSplits := mpi.WorldSize() - 1
	leafList := make([]*streets.StreetGraph, 0)
	for rank := 0; rank <= rectangularSplits; rank++ {
		if rank == 0 {
			continue
		}
		log.Debug().Msgf("[%d] Setting up leaf (WorldSize: %d)", taskID, mpi.WorldSize())
		// rank means taskID
		l, err := setupLeaf(jsonPath, rootGraph, rectangularSplits, rank, rank)
		if err != nil {
			log.Error().Msgf("[%d] Failed to setup leaf", taskID)
			return
		}
		leafList = append(leafList, l)
	}

	log.Info().Msgf("[%d] Leaf list length: %d", taskID, len(leafList))

	var leafLookup = make(map[int]int) // [vertexID] => leafID
	edges, err := rootGraph.Graph.Edges()
	if err != nil {
		log.Error().Err(err).Msg("Failed to get edges")
		return
	}

	for _, graph := range leafList {
		for _, edge := range edges {
			src := edge.Source
			dest := edge.Target
			if graph.VertexExists(src) {
				leafLookup[src] = graph.ID
			}
			if graph.VertexExists(dest) {
				leafLookup[dest] = graph.ID
			}
		}
	}

	log.Debug().Msgf("[%d] Leaf lookup: %d->%v", taskID, 28095826, leafLookup[28095826])

	if taskID == 0 {
		size, err := rootGraph.Graph.Size()
		if err != nil {
			log.Error().Err(err).Msg("Failed to get size of graph")
			return
		}
		log.Info().Msgf("Number of vertices: %d", size)
		m := streets.NewMPI(0, *comm, rootGraph)

		// I.4 root process will emit vehicles initially
		for _, vehicle := range vehicleList {
			err = m.EmitVehicle(*vehicle, leafLookup)
			if err != nil {
				log.Error().Err(err).Msg("Failed to emit vehicle")
				return
			}
		}

		// I.5 root process will listen for incoming requests
		var wg sync.WaitGroup

		wg.Add(1)
		go func(wg *sync.WaitGroup) {
			defer wg.Done()
			err, done := ListenForLengthRequest(err, m)
			if err != nil {
				log.Error().Err(err).Msg("Failed to listen for length request")
				return
			}
			if done {
				return
			}
		}(&wg) // TODO: add done channel?
		// TODO: check if parameter is correct or if it should be a pointer
		log.Info().Msgf("[%d] Waiting for length request", taskID)

		wg.Add(1)
		go func(wg *sync.WaitGroup) {
			defer wg.Done()
			if ListenForReceiveAndSendRequest(err, m, leafLookup) {
				return
			}
		}(&wg)
		// TODO: check if parameter is correct or if it should be a pointer
		log.Info().Msgf("[%d] Waiting for receive and send request", taskID)

		wg.Wait()
	} else {
		log.Info().Msgf("[%d] Starting leaf", taskID)
		m := streets.NewMPI(taskID, *comm, rootGraph)
		leaf := leafList[taskID-1]
		size, err := leaf.Graph.Size()
		if err != nil {
			log.Error().Err(err).Msgf("[%d] Failed to get size of graph", taskID)
			return
		}
		log.Info().Msgf("[%d] Starting leaf size: %d", taskID, size)

		for {
			vehicleOnLeaf, err := m.ReceiveVehicleOnLeaf() // II.1 & II.2
			if err != nil {
				log.Error().Err(err).Msgf("[%d] Failed to receive vehicle on leaf", taskID)
				return
			}
			vehicleOnLeaf.MarkedForDeletion = false // II.3

			length, err := m.AskRootForEdgeLength(vehicleOnLeaf.PrevID, vehicleOnLeaf.NextID) // II.4
			if err != nil {
				log.Error().Err(err).Msgf("[%d] Failed to ask root for edge length", taskID)
				return
			}
			vehicleOnLeaf.Delta += length // II.5

			// TODO: add previous edge length?
			go driveVehicle(vehicleOnLeaf, leaf, taskID, m)
		}

	}
}

func ListenForReceiveAndSendRequest(err error, m *streets.MPI, lookupTable map[int]int) bool {
	for {
		// I.5.b root process will listen for incoming vehicles and send them to the leaf
		err = m.ReceiveAndSendVehicleOverRoot(lookupTable)
		if err != nil {
			log.Error().Err(err).Msg("Failed to receive vehicle on root from leaf")
			return true
		}
	}
}

func ListenForLengthRequest(err error, m *streets.MPI) (error, bool) {
	for {
		// I.5.a root process will listen for incoming requests for edge length
		// TODO: make async
		err = m.RespondToEdgeLengthRequest()
		if err != nil {
			log.Error().Err(err).Msg("Failed to respond to edge length request")
			return nil, true
		}
	}
	return err, false
}

func driveVehicle(vehicleOnLeaf streets.Vehicle, l *streets.StreetGraph, taskID int, m *streets.MPI) bool {
	vehicleOnLeaf.StreetGraph = l // II.5.1

	// update nodes after graph transition II.5.2 -> shift the array
	vehicleOnLeaf.PrevID = vehicleOnLeaf.GetNextID(vehicleOnLeaf.PrevID)
	vehicleOnLeaf.NextID = vehicleOnLeaf.GetNextID(vehicleOnLeaf.PrevID)

	for {
		if vehicleOnLeaf.IsParked { // II.7.1
			log.Info().Msgf("[%d] Vehicle %s is parked", taskID, vehicleOnLeaf.ID) // II.10
			break
		} else if vehicleOnLeaf.MarkedForDeletion { // II.7.2
			log.Debug().Msgf("[%d] Vehicle %s is marked for deletion", taskID, vehicleOnLeaf.ID)
			err := m.SendVehicleToRoot(vehicleOnLeaf) // II.9
			if err != nil {
				log.Error().Err(err).Msgf("[%d] Failed to send vehicle to root", taskID)
				return true
			}
			break
		}
		vehicleOnLeaf.Step() // II.8
	}
	return false
}

func setupLeaf(jsonPath *string, rootGraph *streets.StreetGraph, rectangularSplits int, i int, taskID int) (*streets.StreetGraph, error) {
	log.Debug().Msgf("[%d] i=%d", taskID, i)
	gb := streets.NewGraphBuilder().FromJsonFile(*jsonPath).IsLeaf(rootGraph, taskID).NumberOfRects(rectangularSplits)
	gb = gb.PickRect(i - 1).DivideGraphsIntoRects().FilterForRect()
	gb = gb.SetTopRightBottomLeftVertices()
	leafGraph, err := gb.Build()
	if err != nil {
		log.Error().Err(err).Msg("Failed to build graph")
		return nil, err
	}
	size, err := leafGraph.Graph.Size()
	if err != nil {
		log.Error().Err(err).Msg("Failed to get size of graph")
		return nil, err
	}
	log.Info().Msgf("[%d] Number of vertices: %d", taskID, size)
	return leafGraph, nil
}

func runWithGoRoutines(vehicleList []*streets.Vehicle) {
	var wg sync.WaitGroup
	for _, vehicle := range vehicleList {
		wg.Add(1)
		go func(wg *sync.WaitGroup, vehicle *streets.Vehicle) {
			vehicle.Drive()
			wg.Done()
		}(&wg, vehicle)
	}
	wg.Wait()
}

func runSequentially(vehicleList []*streets.Vehicle) {
	for _, vehicle := range vehicleList {
		vehicle.Drive()
	}
}

func connectVehiclesToGraph(n *int, rootGraph *streets.StreetGraph, minSpeed *float64, maxSpeed *float64, vehicleList []*streets.Vehicle) bool {
	for i := 0; i < *n; i++ {
		v, err := rootGraph.AddVehicle(*minSpeed, *maxSpeed)
		if err != nil {
			return true
		}
		vehicleList[i] = v
	}
	return false
}

func setupLogging(debug *bool) {
	// Logging
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	if *debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	runLogFile, _ := os.OpenFile(
		"main.log",
		os.O_APPEND|os.O_CREATE|os.O_WRONLY,
		0o664,
	)
	multi := zerolog.MultiLevelWriter(os.Stdout, runLogFile)
	log.Logger = zerolog.New(multi).With().Timestamp().Logger()
}
