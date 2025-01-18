# Cryptix Stratum Adapter


This is a lightweight daemon that allows mining to a local (or remote)
cryptix node using stratum-base miners. It is up to the community to
build a stratum based miner, the original built-in miner is using gRPC
interface.

## Browser Interface
Here is a lightfull Browser Interface:

[Cryptix Stratum Browser](https://github.com/cryptix-network/cryptix-stratum-bridge-browser-monitor)
## Mining

You can use SRB Miner, its supported for Cryptix and the Bridge/ Pools:
[SRB Miner](https://github.com/doktor83/SRBMiner-Multi/releases)

For local Node, http Node or Pool Mining you can use:
[Cryptix-Miner](https://github.com/cryptix-network/cryptix-miner)


## Hive Setup
[detailed instructions here](hive-setup.md) 


# Features:

Shares-based work allocation with miner-like periodic stat output:

![image](https://user-images.githubusercontent.com/59971111/191983487-479e19ec-a8cb-4edb-afc4-55a1165e79fc.png)



Optional monitoring UI:

https://github.com/Cryptix/cryptix-stratum-bridge/blob/main/monitoring-setup.md

![image](https://user-images.githubusercontent.com/59971111/192025446-f20d74a5-f9e0-4290-b98b-9f56af8f23b4.png)

![image](https://user-images.githubusercontent.com/59971111/191980688-2d0faf6b-d551-4880-a316-de2303cfeb7d.png)


Prometheus API:

If the app is run with the `-prom={port}` flag the application will host stats on the port specified by `{port}`, these stats are documented in the file [prom.go](src/cryptixstratum/prom.go). This is intended to be use by prometheus but the stats can be fetched and used independently if desired. `curl http://localhost:2114/metrics | grep py_` will get a listing of current stats. All published stats have a `py_` prefix for ease of use.

```
user:~$ curl http://localhost:2114/metrics | grep py_
# HELP cytx_estimated_network_hashrate_gauge Gauge representing the estimated network hashrate
# TYPE cytx_estimated_network_hashrate_gauge gauge
cytx_estimated_network_hashrate_gauge 2.43428982879776e+14
# HELP cytx_network_block_count Gauge representing the network block count
# TYPE cytx_network_block_count gauge
cytx_network_block_count 271966
# HELP cytx_network_difficulty_gauge Gauge representing the network difficulty
# TYPE cytx_network_difficulty_gauge gauge
cytx_network_difficulty_gauge 1.2526479386202519e+14
# HELP cytx_valid_share_counter Number of shares found by worker over time
# TYPE cytx_valid_share_counter counter
cytx_valid_share_counter{ip="192.168.0.17",miner="SRBMiner-MULTI/2.4.4",wallet="cryptix:qzk3uh2twkhu0fmuq50mdy3r2yzuwqvstq745hxs7tet25hfd4egcafcdmpdl",worker="002"} 276
# HELP cytx_worker_job_counter Number of jobs sent to the miner by worker over time
# TYPE cytx_worker_job_counter counter
cytx_worker_job_counter{ip="192.168.0.17",miner="SRBMiner-MULTI/2.4.4",wallet="cryptix:qzk3uh2twkhu0fmuq50mdy3r2yzuwqvstq745hxs7tet25hfd4egcafcdmpdl",worker="002"} 3471

```

# Install

## Docker All-in-one

Note: This does requires that docker is installed.

  

`docker compose -f docker-compose-all.yml up -d` will run the bridge with default settings. This assumes a local cryptix node with default port settings and exposes port 5555 to incoming stratum connections.

  

This also spins up a local prometheus and grafana instance that gather stats and host the metrics dashboard. Once the services are up and running you can view the dashboard using `http://127.0.0.1:3000/d/x7cE7G74k/monitoring`

Default grafana user/pass: admin/admin

Most of the stats on the graph are averaged over an hour time period, so keep in mind that the metrics might be inaccurate for the first hour or so that the bridge is up.


## Docker (non-compose)

Note: This does not require pulling down the repo, it only requires that docker is installed.

`docker run -p 5555:5555 cryptix/cryptix-stratum-bridge:latest --log=false` will run the bridge with default settings. This assumes a local cryptix node with default port settings and exposes port 5555 to incoming stratum connections.


Detailed:

`docker run -p {stratum_port}:5555 cryptix/cryptix-stratum-bridge  --log=false --cryptix={cryptix_address} --stats={false}` will run the bridge targeting a cryptix node at {cryptix_address}. stratum port accepting connections on {stratum_port}, and only logging connection activity, found blocks, and errors

  

## Manual build

Install go 1.18 using whatever package manager is approprate for your system

  

run `cd cmd/cryptixbridge;go build .`

  

Modify the config file in ./cmd/bridge/config.yaml with your setup, the file comments explain the various flags

  

run `./cryptixbridge` in the `cmd/cryptixbridge` directory

  

all-in-one (build + run) `cd cmd/cryptixbridge/;go build .;./cryptixbridge`
