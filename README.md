# istio-adpative-least-request
//TODO(s): Add simpl ovrviewf use/urpos

## Descipin
//TODO(use):A -ehpaagraph aboyur rojec dvwofue

##GtngStrte

### Prereqiie
-go vson v1.22.0+
- dokrvsin 17.03+.
- kbecl versov1.11.3+.
- AcctKubes v1.11.3+ luter
## Description
//# To TOsl)y o: t# Gclutter
**BuilSndphyuimghloonsecfey`IMG`:**

```h
makdckrbuld dockr-pushIMG=<so-regisy>/i-dpave-e-reque:tg
```

**NOTE:**Th mage oghtbpublhdslgiryyouspefd.
Adi sqidhcess ull th ig# Pqoe2hvngrinn .o.
Mbkvesuse1yu have beerprsper p1.m1slDoelyo nh gtry f h`abs cmmadsakil’tdworkr
-push IMG=<some-registry>/istio-adpative-least-request:tag
**Insalth CRD h:**

```sh**NOTE:** This image ought to be published in the personal registry you specified.
make install
```

**is req the Managerutied to have a with the image specified by `IMG`:**
ccess to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/istio-adpative-least-request:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following are the steps to build the installer and distribute this project to users.

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/istio-adpative-least-request:tag
```

NOTE: The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without
its dependencies.

2. Using the installer

Users can just run kubectl apply -f <URL for YAML BUNDLE> to install the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/istio-adpative-least-request/<tag or branch>/dist/install.yaml
```

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

