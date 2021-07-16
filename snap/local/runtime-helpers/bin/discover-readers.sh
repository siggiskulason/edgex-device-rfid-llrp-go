#!/bin/bash

# note that it's important to run this using snapcraft-runner, to set the shared libraries path correctly

# TODO: Only update the subnets if they have not been set already?
# TODO: optimize by using snapctl get to check if both interfaces are connected, before running. 
#       This script requires both network-control and network-observe. 
#	It will fail silently if they have not been
#       both connected, so it's ok to run this script in hooks for both interfaces.


CURL=$SNAP/usr/bin/curl

setup() {

    CONSUL_URL=${CONSUL_URL:-http://localhost:8500}
    url="${CONSUL_URL}/v1/kv/edgex/devices/1.0/edgex-device-rfid-llrp/Driver/DiscoverySubnets"
    
    # find all online non-virtual network interfaces, separated by `|` for regex matching. ie. (eno1|eno2|eno3|...)
    ifaces=$(
        find /sys/class/net -mindepth 1 -maxdepth 2 \
            -not -lname '*devices/virtual*' \
            -execdir grep -q 'up' "{}/operstate" \; \
            -printf '%f|'
    )
    echo "Interfaces:"
    echo $ifaces


    # print all ipv4 subnets, filter for just the ones associated with our physical interfaces
    # grab the unique ones and join them by commas
    subnets=$(
        ip -4 -o route list scope link | \
        sed -En "s/ dev (${ifaces::-1}).+//p" | \
        sort -u | \
        paste -sd, -
    )

    printf "\e[1m%18s\e[0m: %s\n" "Subnets" "${subnets}"
    if [ -z "${subnets}" ]; then
        echo "Error, no subnets detected" 1>&2
        exit 0
    fi

    code=$($CURL -X PUT --data "${subnets}" -w "%{http_code}" -o /dev/null -s "${url}")
    if [ "${code}" -ne 200 ]; then
        echo -e "${red}${bold}Failed!${normal} curl returned a status code of '${bold}${code}'${clear}"
        exit $((code))
    fi

	
    echo "Start device discovery"
    sleep 1
    $CURL -X POST http://localhost:51992/api/v1/discovery
    echo "\n"
 }

setup
exit 0
