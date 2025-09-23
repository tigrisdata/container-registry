#!/bin/bash
set -e

if [ -z "$REDIS_ROLE" ]; then
    echo "ERROR: REDIS_ROLE environment variable is not set"
    echo "Valid roles: primary, replica, sentinel, cluster-node, cluster-starter"
    exit 1
fi

case "$REDIS_ROLE" in
primary | replica | sentinel | cluster-node | cluster-starter)
    echo "Starting Redis in role: $REDIS_ROLE"
    ;;
*)
    echo "ERROR: Invalid REDIS_ROLE: $REDIS_ROLE"
    echo "Valid roles: primary, replica, sentinel, cluster-node, cluster-starter"
    exit 1
    ;;
esac

# Set default values for environment variables
export REDIS_ANNOUNCE_IP="${REDIS_ANNOUNCE_IP:-$(hostname)}"
export CLUSTER_INIT_MAX_ATTEMPTS="${CLUSTER_INIT_MAX_ATTEMPTS:-30}"
export CLUSTER_STARTER_KEEP_ALIVE="${CLUSTER_STARTER_KEEP_ALIVE:-true}"

REDIS_AUTH_TEMPLATE='
user default off
user ${REDIS_USERNAME} on >${REDIS_PASSWORD} allcommands allkeys'

SENTINEL_AUTH_TEMPLATE='
user default off
user ${REDIS_SENTINEL_USERNAME} on >${REDIS_SENTINEL_PASSWORD} +@all'

# Doc: https://github.com/redis/redis/blob/unstable/redis.conf
PRIMARY_TEMPLATE='
loglevel verbose

appendonly no
save ""

repl-diskless-sync yes
repl-diskless-sync-delay 0
repl-diskless-load swapdb

protected-mode no

replica-announce-ip ${REDIS_ANNOUNCE_IP}
replica-announce-port 6379

min-replicas-to-write 0
min-replicas-max-lag 10'

# Doc: https://github.com/redis/redis/blob/unstable/redis.conf
REPLICA_TEMPLATE='
loglevel verbose

appendonly no
save ""

repl-diskless-sync yes
repl-diskless-sync-delay 0
repl-diskless-load swapdb

protected-mode no

replicaof ${REDIS_PRIMARY_HOST} 6379

replica-announce-ip ${REDIS_ANNOUNCE_IP}
replica-announce-port 6379'

# Doc: https://github.com/redis/redis/blob/unstable/redis.conf
SENTINEL_TEMPLATE='
port 6379
loglevel verbose
dir /tmp

sentinel monitor ${REDIS_MAIN_NAME} ${REDIS_MONITOR_HOST} 6379 1
sentinel announce-ip ${REDIS_ANNOUNCE_IP}
# NOTE(prozlach): We need to re-use the Redis port to make Gitlab
# Runner healthchecks happy:
sentinel announce-port 6379
sentinel announce-hostnames no
sentinel resolve-hostnames yes

sentinel down-after-milliseconds ${REDIS_MAIN_NAME} 1500
sentinel failover-timeout ${REDIS_MAIN_NAME} 3000
sentinel parallel-syncs ${REDIS_MAIN_NAME} 1

sentinel deny-scripts-reconfig yes'

# Doc: https://github.com/redis/redis/blob/unstable/redis.conf
CLUSTER_NODE_TEMPLATE='
loglevel verbose

cluster-enabled yes
cluster-config-file /etc/redis-nodes.conf
cluster-announce-ip ${REDIS_ANNOUNCE_IP}
cluster-announce-port 6379
cluster-node-timeout 2000

appendonly no
save ""

repl-diskless-sync yes
repl-diskless-sync-delay 0
repl-diskless-load swapdb

protected-mode no
min-replicas-to-write 0
min-replicas-max-lag 10'

# Function to handle primary role
configure_as_primary() {
    echo "Configuring Redis as primary node..."
    local config_file="/etc/redis-server.conf"
    local template="$PRIMARY_TEMPLATE"

    if [ -n "$REDIS_USERNAME" ] && [ -n "$REDIS_PASSWORD" ]; then
        template="${PRIMARY_TEMPLATE}${REDIS_AUTH_TEMPLATE}"
    fi

    echo "$template" | envsubst >"$config_file"
    exec redis-server "$config_file"
}

# Function to handle replica role
configure_as_replica() {
    echo "Configuring Redis as replica node..."

    if [ -z "$REDIS_PRIMARY_HOST" ]; then
        echo "ERROR: REDIS_PRIMARY_HOST must be set for replica role"
        exit 1
    fi

    local config_file="/etc/redis-server.conf"
    local template="$REPLICA_TEMPLATE"

    if [ -n "$REDIS_USERNAME" ] && [ -n "$REDIS_PASSWORD" ]; then
        template="${REPLICA_TEMPLATE}${REDIS_AUTH_TEMPLATE}"
    fi

    echo "$template" | envsubst >"$config_file"
    exec redis-server "$config_file"
}

# Function to handle sentinel role
configure_as_sentinel() {
    echo "Configuring Redis Sentinel..."

    export REDIS_MONITOR_HOST="${REDIS_MONITOR_HOST:-redis-node1}"

    local config_file="/etc/redis-sentinel.conf"
    local template="$SENTINEL_TEMPLATE"

    if [ -n "$REDIS_SENTINEL_USERNAME" ] && [ -n "$REDIS_SENTINEL_PASSWORD" ]; then
        template="${SENTINEL_TEMPLATE}${SENTINEL_AUTH_TEMPLATE}"
    fi

    echo "$template" | envsubst >"$config_file"
    exec redis-sentinel "$config_file"
}

# Function to handle cluster node role
configure_as_cluster_node() {
    echo "Configuring Redis as cluster node..."
    local config_file="/etc/redis-cluster.conf"

    echo "$CLUSTER_NODE_TEMPLATE" | envsubst >"$config_file"
    exec redis-server "$config_file"
}

# Function to handle cluster starter role
configure_as_cluster_starter() {
    echo "Starting Redis Cluster initialization..."

    if [ -z "$REDIS_CLUSTER_NODES" ]; then
        echo "ERROR: REDIS_CLUSTER_NODES must be set for cluster-starter role"
        echo "Example: REDIS_CLUSTER_NODES='redis-node1,redis-node2,redis-node3'"
        exit 1
    fi

    IFS=',' read -ra REDIS_NODES <<<"$REDIS_CLUSTER_NODES"

    # Function to wait for Redis node to be ready
    wait_for_redis() {
        local node=$1
        local max_attempts=$CLUSTER_INIT_MAX_ATTEMPTS
        local attempt=0

        echo "Waiting for $node to be ready..."

        while [ $attempt -lt $max_attempts ]; do
            if redis-cli -h "$node" -p 6379 ping 2>/dev/null | grep -q PONG; then
                echo "$node is ready!"
                return 0
            fi

            attempt=$((attempt + 1))
            sleep 1
        done

        echo "ERROR: $node is not responding after $max_attempts attempts"
        return 1
    }

    # Wait for all nodes to be ready
    for node in "${REDIS_NODES[@]}"; do
        if ! wait_for_redis "$node"; then
            echo "Cluster initialization failed - $node is not ready"
            exit 1
        fi
    done

    # Build node list with IPs
    NODE_LIST=""
    for node in "${REDIS_NODES[@]}"; do
        # Get the IP address of the node
        NODE_IP=$(getent hosts "$node" | awk '{ print $1 }')
        if [ -z "$NODE_IP" ]; then
            echo "Failed to resolve IP for $node"
            exit 1
        fi

        if [ -n "$NODE_LIST" ]; then
            NODE_LIST="$NODE_LIST "
        fi
        NODE_LIST="${NODE_LIST}${NODE_IP}:6379"
        echo "Resolved $node to $NODE_IP"
    done

    echo "Node list with IPs: $NODE_LIST"
    echo "Creating Redis cluster..."

    # Create cluster with 0 replicas (hardcoded)
    redis-cli --cluster create $NODE_LIST --cluster-replicas 0 --cluster-yes

    if [ $? -eq 0 ]; then
        echo "Redis cluster created successfully!"
    else
        echo "Failed to create Redis cluster"
        exit 1
    fi

    echo "Verifying cluster status..."
    sleep 1

    for node in "${REDIS_NODES[@]}"; do
        echo "Checking node: $node"
        redis-cli -h "$node" -p 6379 cluster info | grep cluster_state
    done

    echo "Cluster nodes configuration:"
    redis-cli -h "${REDIS_NODES[0]}" -p 6379 cluster nodes

    echo "Redis cluster initialization completed!"

    echo "Starting Redis server to make health checks happy..."
    exec redis-server
}

# Main execution
case "$REDIS_ROLE" in
primary)
    configure_as_primary
    ;;
replica)
    configure_as_replica
    ;;
sentinel)
    configure_as_sentinel
    ;;
cluster-node)
    configure_as_cluster_node
    ;;
cluster-starter)
    configure_as_cluster_starter
    ;;
*)
    echo "Error: Unknown REDIS_ROLE: $REDIS_ROLE"
    exit 1
    ;;
esac
