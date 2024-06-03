# Cloud Run `tcpdump` sidecar

This repository contains the source code to create a container image containing `tcpdump` to perform packet capture in Cloud Run multi-container deployments.

## Motivation

During development, it is often useful to perform packet capture to troubleshoot specific network related issues/conditions.

This container image is to be used as a sidecar of the Cloud Run main –*ingress*– container in order to perform a packet capture using `tcpdump` within the same network namespace.

The sidecar approach enables decoupling from the main –*ingress*– container so that it does not require any modifications to perform a packet capture; additionally, sidecars use their own resources which allows `tcpdump` to not compete with the main app resources allocation.

> **NOTE**: the main –*ingres*– container is the one to which all ingress traffic ( HTTP Requests ) is delivered to; for Cloud Run services, this is typically your APP container.

## Building blocks

- [Ubuntu 22.04 official docker image](https://hub.docker.com/_/ubuntu)
- [`tcpdump`](https://www.tcpdump.org/) installed from [Ubuntu's official repository](https://packages.ubuntu.com/search?keywords=tcpdump) to perform packet captures.
- [GCSFuse](https://github.com/GoogleCloudPlatform/gcsfuse) to mount the GCS Bucket used to export **PCAP files**.
- [Go Supervisord](https://github.com/ochinchina/supervisord) to orchestrate startup processes execution.
- [fsnotify](https://github.com/fsnotify/fsnotify) to listen for filesystem events.
- [gocron](https://github.com/go-co-op/gocron) to schedule execution of `tcpdump`.
- Uber's [zap](https://github.com/uber-go/zap) blazing fast, structured, leveled logging for Go.
- [Docker Engine](https://docs.docker.com/engine/) and [Docker CLI](https://docs.docker.com/engine/reference/commandline/cli/) to build the sidecar container image.
- [Cloud Run](https://cloud.google.com/run/docs/deploying#multicontainer-yaml) **gen2** [execution environment](https://cloud.google.com/run/docs/about-execution-environments).

## How it works

The sidecar uses:

-    **`tcpdump`** to capture packets. All containers use the same network namestap and so this sidecar captures packets from all containers within the same deployment.

-    [**`tcpdumpw`**](tcpdumpw/main.go) to execute `tcpdump` and generate **PCAP files**; optionally, schedule recurring `tcpdump` executions.

-    [**`pcap-fsnotify`**](pcap-fsnotify/main.go) to listen for newly created **PCAP files**, optionally compress PCAPs ( _**recommended**_ ) and move them into Cloud Storage mount point.

-    **GCSFuse** to mount a Cloud Storage Bucket to move compressed **PCAP files** into.

     > **PCAP files** are moved from the sidecar's in-memory filesystem into the mounted Cloud Storage Bucket.

## How to build the sidecar

1. Define the `PROJECT_ID` environment variable; i/e: `export PROJECT_ID='...'`.

2. Clone this repository: 

     ```sh
     git clone --depth=1 --branch=main --single-branch https://github.com/gchux/cloud-run-tcpdump.git
     ```

     > If you prefer to let Cloud Build perform all the tasks, go directly to build [using Cloud Build](#using-cloud-build)

3. Move into the repository local directory: `cd cloud-run-tcpdump`.

Continue with one of the following alternatives:

### Using a local environment or [Cloud Shell](https://cloud.google.com/shell/docs/launching-cloud-shell)

4. Build and push the `tcpdump` sidecar container image:

     ```sh
     export TCPDUMP_IMAGE_URI='...' # this is usually Artifact Registry
     ./docker_build ${TCPDUMP_IMAGE_URI}
     docker push ${TCPDUMP_IMAGE_URI}
     ```

### Using [Cloud Build](https://cloud.google.com/build/docs/build-config-file-schema)

This approach assumes that Artifact Registry is available in `PROJECT_ID`.

> If you skipped step (2), clone the [**gcb** branch](https://github.com/gchux/cloud-run-tcpdump/tree/gcb):
>
> ```sh
> git clone --depth=1 --branch=gcb --single-branch https://github.com/gchux/cloud-run-tcpdump.git
> ```

4. Define the following environment variables:

     ```sh
     export REPO_LOCATION='...' # Artifact Registry Docker repository location
     export REPO_NAME='...' # Artifact Registry Docker repository name
     export IMAGE_NAME='...' # container image name; i/e: `sidecars/tcpdump` 
     export IMAGE_VERSION='...' # container image version; i/e: `latest`
     export TCPDUMP_IMAGE_URI="${REPO_LOCATION}-docker.pkg.dev/${PROJECT_ID}/${IMAGE_NAME}:${IMAGE_VERSION}" # using Artifact Registry
     ```

5. Build and push the `tcpdump` sidecar container image using Cloud Build: 

     ```sh
     gcloud builds submit \
       --project=${PROJECT_ID} \
       --config=$(pwd)/cloudbuild.yaml \
       --substitutions="_REPO_LOCATION=${REPO_LOCATION},_REPO_NAME=${REPO_NAME},_IMAGE_NAME=${IMAGE_NAME},_IMAGE_VERSION=${IMAGE_VERSION}' $(pwd)
     ```

> See the full list of available flags for `gcloud builds submit`: https://cloud.google.com/sdk/gcloud/reference/builds/submit

## How to deploy to Cloud Run

1. Define environment variables to be used during Cloud Run service deployment:

     ```sh
     export SERVICE_NAME='...'
     export SERVICE_REGION='...' # GCP Region: https://cloud.google.com/about/locations
     export SERVICE_ACCOUNT='...' # Cloud Run service's identity
     export INGRESS_CONTAINER_NAME='...'
     export INGRESS_IMAGE_URI='...'
     export INGRESS_PORT='...'
     export TCPDUMP_SIDECAR_NAME='...'
     export TCPDUMP_IMAGE_URI='...' # same as the one used to build the sidecar container image
     export GCS_BUCKET='...'        # the name of the Cloud Storage Bucket to mount
     export PCAP_FILTER='...'       # the BPF filter to use; i/e: `tcp port 443`
     export PCAP_ROTATE_SECS='...'  # how often to rocate PCAP files; default is `60` seconds 
     export PCAP_SNAPSHOT_LENGTH='...'  # see: https://www.tcpdump.org/manpages/tcpdump.1.html#:~:text=%2D%2D-,snapshot%2Dlength,-%3Dsnaplen ; default is `0` bytes
     ```

2. Deploy the Cloud Run service including the `tcpdump` sidecar:

     ```sh
     gcloud beta run deploy ${SERVICE_NAME} \
       --project=${PROJECT_ID} \
       --region=${SERVICE_REGION} \
       --execution-environment=gen2 \ # execution environment gen2 is mandatory
       --service-account=${SERVICE_ACCOUNT} \
       --container=${INGRESS_CONTAINER_NAME}-1 \
       --image=${INGRESS_IMAGE_URI} \
       --port=${INGRESS_PORT} \
       --container=${TCPDUMP_SIDECAR_NAME}-1 \
       --image=${TCPDUMP_IMAGE_URI} \
       --cpu=1 --memory=1G \
       --set-env-vars="GCS_BUCKET=${GCS_BUCKET},PCAP_FILTER=${PCAP_FILTER},PCAP_ROTATE_SECS=${PCAP_ROTATE_SECS},PCAP_SNAPSHOT_LENGTH=${PCAP_SNAPSHOT_LENGTH}" \
       --depends-on=${INGRESS_CONTAINER_NAME}-1
     ```

> See the full list of available falgs for `gcloud run deploy` at https://cloud.google.com/sdk/gcloud/reference/run/deploy

## Available configurations

The `tcpdump` sidecar accespts the following environment variables:

-    `GCS_BUCKET`: (STRING, **required**) the name of the Cloud Storage Bucket to be mounted and used to store **PCAP files**.

-    `PCAP_FILTER`: (STRING, **required**) standard `tcpdump` bpf filters to scope the packet capture to specific traffic; i/e: `tcp`.

-    `PCAP_SNAPSHOT_LENGTH`: (NUMBER, *optional*) bytes of data from each packet rather than the default of 262144 bytes; default value is `0`.

-    `PCAP_ROTATE_SECS`: (NUMBER, *optional*) how often to rotate **PCAP files** created by `tcpdump`; default value is `60` seconds.

-    `GCS_MOUNT`: (STRING, *optional*) where in the sidecar in-memory filesystem to mount the Cloud Storage Bucket; default value is `/pcap`.

-    `PCAP_FILE_EXT`: (STRING, *optional*) extension to be used for **PCAP files**; default value is `pcap`.

-    `PCAP_COMPRESS`: (BOOLEAN, *optional*) whether to compress **PCAP files** or not; default value is `true`.

### Advanced configurations

More advanced use cases may benefit from scheduling `tcpdump` executions. Use the following environment variables to configure scheduling:

-    `PCAP_USE_CRON`: (BOOLEAN, *optional*) whether to enable scheduling of `tcpdump` executions; default value is `false`.

-    `PCAP_CRON_EXP`: (STRING, *optional*) [`cron` expression](https://man7.org/linux/man-pages/man5/crontab.5.html) used to configure scheduling `tcpdump` executions. 
     
     - **NOTE**: if `PCAP_USE_CRON` is set to `true`, then `PCAP_CRON_EXP` is required. See https://crontab.cronhub.io/ to get help with `crontab` expressions.

-    `PCAP_TIMEZONE`: (STRING, *optional*) the Timezone ID used to configure scheduling of `tcpdump` executions using `PCAP_CRON_EXP`; default value is `UTC`.

-    `PCAP_TIMEOUT_SECS`: (NUMBER, *optional*) seconds `tcpdump` execution will last; devault value is `0`: execution will not be stopped.

     - **NOTE**: if `PCAP_USE_CRON` is set to `true`, you should set this value to less than the time in seconds between scheduled executions.

## Considerations

-    The Cloud Storage Bucket mounted by the `tcpdump` sidecar is not accessible by the main –ingress– container.

-    Processes running in the `tcpdump` sidecar are not visible to the main –*ingress*– container ( or any other container ); similarly, the `tcpdump` sidecar doesn't have visibility of processes running in other containers.

-    Packet capturing using `tcpdump` requires raw sockets, which is only available for Cloud Run **gen2** execution environment as it offers [full Linux compatibility](https://cloud.google.com/run/docs/about-execution-environments#:~:text=second%20generation%20execution%20environment%20provides%20full%20Linux%20compatibility).

-    All **PCAP files** will be stored within the Cloud Storage Bucket with the following "*hierarchy*": `PROJECT_ID`/`SERVICE_NAME`/`GCP_REGION`/`REVISION_NAME`/`INSTANCE_STARTUP_TIMESTAMP`/`INSTANCE_ID`.

     > this hierarchy guarantees that **PCAP files** are easily indexable and hard to override by multiple deployments/instances. It also simplifies deleting no longer needed PCAPs from specific deployments/instances.

-    When defining `PCAP_ROTATE_SECS`, keep in mind that the current **PCAP file** is temporarily stored in the sidecar in-memory filesystem. This means that if your APP is network intensive:

     -    The longer it takes to rotate the current **PCAP file**, the larger the current **PCAP file** will be, so...
         
     -    Larger **PCAP files** will require more memory to temporarily store the current one before offloading it into the Cloud Storage Bucket.

-    When defining `PCAP_SNAPSHOT_LENGTH`, keep in mind that a large value will result in larget **PCAP files**; additionally, you may not need to ispect the data, just the packet headers.

-    Keep in mind that every Cloud Run instance will produce its own set of **PCAP files**, so for troubleshooting purposes, it is best to define a low Cloud Run [maximum number of instances](https://cloud.google.com/run/docs/configuring/max-instances).

     > It is equally important to define a well scoped BPF filter in order to capture only the required packets and skip everything else. The `tcpdump` flag [--snapshot-length](https://www.tcpdump.org/manpages/tcpdump.1.html) is also useful to limit the bytes of data to capture from each packet.

-    Packet capturing is always on while the instance is available, so it is best to rollback to a non packet capturing revision and delete the packet-capturing one after you have captured all the required traffic.

-    The full packet capture from a Cloud Run instance will be composed out of multiple smaller ( optionally compressed ) **PCAP files**. Use a tool like [mergecap](https://www.wireshark.org/docs/man-pages/mergecap.html) to combine them into one.

-    In order to be able to mount the Cloud Storage Bucket and store **PCAP files**, [Cloud Run's identity](https://cloud.google.com/run/docs/securing/service-identity) must have proper [roles/permissions](https://cloud.google.com/storage/docs/access-control/iam-permissions).

-    The `tcpdump` sidecar is intended to be used for troubleshooting purposes only. While the `tcpdump` sidecar has its own set of resources, storing bytes from **PCAP files** in Cloud Storage introduces additional costs ( for both Storage and Networking ).

     -    Set `PCAP_COMPRESS` to `true` to store compressed **PCAP files** and save storage bytes; additionally, use regional Buckets to minize costs.

     -    Whenever possible, use packet capturing scheduling to avoid running `tcpdump` 100% of instance lifetime.

     -    When troubleshooting is complete, deploy a new Revision without the `tcpdump` sidecar to completely disable it.

-    While it is true that [Cloud Storage volume mounts](https://cloud.google.com/run/docs/configuring/services/cloud-storage-volume-mounts) is available in prevew, GCSFuse is used instead to minimize the required configuration to deploy a Revision instrumented with the `tcpdump` sidecar.

     >    **NOTE***: this is also the reason why the base image for the `tcpdump` sidecar is `ubuntu:22.04` and not something lighter like `alpine`. GCSFuse pre-built packages are only available for Debian and RPM based distributions.

## Download and Merge all PCAP Files

1. Use Cloud Logging to look for the entry starting with: `[INFO] - PCAP files available at: gs://`...

     It may be useful to use the following filter:

     ```
     resource.type = "cloud_run_revision"
     resource.labels.service_name = "<cloud-run-service-name>"
     resource.labels.location = "<cloud-run-service-region>"
     "<cloud-run-revision-name>"
     "PCAP files available at:"
     ```

     This entry contains the exact Cloud Storate path to be used to download all the **PCAP files**.

     Copy the full path including the prefix `gs://`, and assign it to the environment variable `GCS_PCAP_PATH`.

2. Download all **PCAP files** using:


     ```sh
     mkdir pcap_files
     cd  pcap_files
     gcloud storage cp ${GCS_PCAP_PATH}/*.gz . # use `${GCS_PCAP_PATH}/*.pcap` if `PCAP_COMPRESS` was set to `false`
     ```

3. If `PCAP_COMPRESS` was set to `true`, uncompress all the **PCAP files**: `gunzip ./*.gz`

4. Merge all **PCAP files** into a single file: `mergecap -w full.pcap -F pcap ./*.pcap`

     > See `mergecap` docs: https://www.wireshark.org/docs/man-pages/mergecap.html
