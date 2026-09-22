# phpray-mcp w kontenerze: serwer MCP mówi po stdio, więc obraz ma sens
# głównie tam, gdzie klient MCP uruchamia `docker run -i`, i w katalogach
# serwerów, które budują obraz, żeby sprawdzić, czy serwer w ogóle wstaje
# i odpowiada na introspekcję.
#
#   docker build -t phpray-mcp src/mcp
#   docker run -i --rm -e PHPRAY_TOKEN=<token konsoli> phpray-mcp
#   docker run -i --rm --network host -e PHPRAY_MODE=local \
#     -v /etc/phpray/collector.toml:/etc/phpray/collector.toml:ro phpray-mcp
#
# Bez żadnej zmiennej serwer i tak wstaje i wypisuje listę narzędzi — dopiero
# wywołanie narzędzia potrzebuje kolektora albo tokenu.

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# Statycznie, bez cgo: obraz końcowy nie ma libc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/phpray-mcp .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/phpray-mcp /usr/local/bin/phpray-mcp
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/phpray-mcp"]
